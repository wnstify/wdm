package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/core"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/internal/security"
	"github.com/wnstify/wdm/internal/state"
	"github.com/wnstify/wdm/internal/system"
	"github.com/wnstify/wdm/pkg/types"
)

func TestInstall_AcquiresAndReleasesRuntimeLockAfterSuccessfulPlanning(t *testing.T) {
	t.Parallel()

	appID := "test-app"
	secondAppID := "second-test-app"
	tcpPort := freeLocalTCPPort(t)
	secondTCPPort := freeLocalTCPPort(t)
	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/test/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
`,
		"templates/test/.env.tmpl": "",
	}, appFixture(appID, tcpPort), appFixture(secondAppID, secondTCPPort))

	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{
			CPUCores:         4,
			TotalMemoryBytes: 8 * gibibyte,
		}, nil
	})
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(&fakeDockerClient{}))

	req := types.InstallRequest{AppID: appID}

	res, err := eng.Install(t.Context(), req, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	lockPath := filepath.Join(stateDir, "runtime.lock")
	require.FileExists(t, lockPath)

	raw, err := os.ReadFile(lockPath)
	require.NoError(t, err)
	var info state.RuntimeLockInfo
	require.NoError(t, json.Unmarshal(raw, &info))
	assert.Equal(t, "install", info.Command)

	// Second app proves the first call released runtime.lock.
	res, err = eng.Install(t.Context(), types.InstallRequest{AppID: secondAppID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)
}

func TestInstall_RejectsEmptyOrUnknownAppID(t *testing.T) {
	t.Parallel()

	catalogFS := catalogFixtureFS(t, appFixture("known-app", freeLocalTCPPort(t)))
	tests := []struct {
		name           string
		appID          string
		wantZeroEvents bool
	}{
		{name: "empty app id", appID: "", wantZeroEvents: true},
		{name: "unknown app id", appID: "missing-app"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
			core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
				return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
			})

			var events int
			onProgress := func(string, float64, string) {
				events++
			}

			res, err := eng.Install(t.Context(), types.InstallRequest{AppID: tt.appID}, onProgress, nil)
			require.Error(t, err)
			assert.Nil(t, res)

			var typedErr *types.Error
			require.ErrorAs(t, err, &typedErr)
			assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
			if tt.wantZeroEvents {
				assert.Zero(t, events, "request validation must refuse before the first progress event")
			}
		})
	}
}

func TestInstall_MapsCatalogReadAndValidationErrorsToVerificationFailed(t *testing.T) {
	t.Parallel()

	t.Run("missing catalog file", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t)
		core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
			return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
		})

		_, err := eng.Install(t.Context(), types.InstallRequest{AppID: "test-app"}, nil, nil)
		require.Error(t, err)

		var typedErr *types.Error
		require.ErrorAs(t, err, &typedErr)
		assert.Equal(t, types.ErrCodeVerificationFailed, typedErr.Code)
		assert.True(t, errors.Is(err, fs.ErrNotExist))
	})

	t.Run("invalid catalog bytes", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t, core.WithCatalog(fstest.MapFS{
			"stable/catalog.yaml": &fstest.MapFile{Data: []byte("apps: [")},
		}))
		core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
			return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
		})

		_, err := eng.Install(t.Context(), types.InstallRequest{AppID: "test-app"}, nil, nil)
		require.Error(t, err)

		var typedErr *types.Error
		require.ErrorAs(t, err, &typedErr)
		assert.Equal(t, types.ErrCodeVerificationFailed, typedErr.Code)
		assert.True(t, errors.Is(err, catalog.ErrCatalogInvalid))
	})
}

func TestInstallPlan_DomainValidationAndNormalization(t *testing.T) {
	t.Parallel()

	app := appFixture("domain-app", freeLocalTCPPort(t))
	app.Placeholders = []catalog.Placeholder{{
		Name:     "DOMAIN",
		Type:     "domain",
		Required: true,
	}}
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

	host := system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}

	validPlan, err := core.PlanInstallForTest(
		eng,
		t.Context(),
		types.InstallRequest{
			AppID:  app.AppID,
			Domain: "ExAmPlE.COM",
		},
		host,
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "example.com", validPlan.ResolvedValues["DOMAIN"])

	cases := []struct {
		name   string
		domain string
	}{
		{name: "wildcard", domain: "*.example.com"},
		{name: "localhost", domain: "localhost"},
		{name: "ipv4", domain: "127.0.0.1"},
		{name: "ipv6", domain: "::1"},
		{name: "scheme shaped", domain: "https://example.com"},
		{name: "non ascii", domain: "münich.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := core.PlanInstallForTest(
				eng,
				t.Context(),
				types.InstallRequest{
					AppID:  app.AppID,
					Domain: tc.domain,
				},
				host,
				nil,
			)
			require.Error(t, err)

			var typedErr *types.Error
			require.ErrorAs(t, err, &typedErr)
			assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
		})
	}
}

func TestResolveTimezoneForTest_Order(t *testing.T) {
	t.Parallel()

	loadLocation := func(name string) (*time.Location, error) {
		if name == "Europe/Prague" || name == "Europe/Warsaw" || name == "Europe/Berlin" {
			return time.UTC, nil
		}
		return nil, fmt.Errorf("invalid location %q", name)
	}

	t.Run("TZ env has precedence", func(t *testing.T) {
		t.Parallel()

		got, err := core.ResolveTimezoneForTest(
			"",
			"",
			core.TimezoneLookupDepsForTest{
				LookupEnv: func(k string) (string, bool) {
					if k == "TZ" {
						return "Europe/Prague", true
					}
					return "", false
				},
				ReadFile:     func(string) ([]byte, error) { return nil, fs.ErrNotExist },
				ReadLink:     func(string) (string, error) { return "", fs.ErrNotExist },
				LoadLocation: loadLocation,
			},
		)
		require.NoError(t, err)
		assert.Equal(t, "Europe/Prague", got)
	})

	t.Run("etc timezone is second", func(t *testing.T) {
		t.Parallel()

		got, err := core.ResolveTimezoneForTest(
			"",
			"",
			core.TimezoneLookupDepsForTest{
				LookupEnv: func(string) (string, bool) { return "", false },
				ReadFile: func(path string) ([]byte, error) {
					if path == "/etc/timezone" {
						return []byte("Europe/Warsaw\n"), nil
					}
					return nil, fs.ErrNotExist
				},
				ReadLink:     func(string) (string, error) { return "", fs.ErrNotExist },
				LoadLocation: loadLocation,
			},
		)
		require.NoError(t, err)
		assert.Equal(t, "Europe/Warsaw", got)
	})

	t.Run("localtime symlink is third", func(t *testing.T) {
		t.Parallel()

		got, err := core.ResolveTimezoneForTest(
			"",
			"",
			core.TimezoneLookupDepsForTest{
				LookupEnv: func(string) (string, bool) { return "", false },
				ReadFile:  func(string) ([]byte, error) { return nil, fs.ErrNotExist },
				ReadLink: func(path string) (string, error) {
					if path == "/etc/localtime" {
						return "/usr/share/zoneinfo/Europe/Berlin", nil
					}
					return "", fs.ErrNotExist
				},
				LoadLocation: loadLocation,
			},
		)
		require.NoError(t, err)
		assert.Equal(t, "Europe/Berlin", got)
	})
}

func TestInstallPlan_PathPlaceholderValidation(t *testing.T) {
	t.Parallel()

	app := appFixture("jellyfin", freeLocalTCPPort(t))
	app.Placeholders = []catalog.Placeholder{{
		Name:     "MEDIA_PATH",
		Type:     "path",
		Required: true,
	}}
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
	host := system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}

	t.Run("rejects relative path", func(t *testing.T) {
		t.Parallel()

		_, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{
				AppID: app.AppID,
				PlaceholderValues: map[string]string{
					"MEDIA_PATH": "relative/path",
				},
			},
			host,
			nil,
		)
		require.Error(t, err)
		assertUsageValidation(t, err)
	})

	t.Run("rejects missing path", func(t *testing.T) {
		t.Parallel()

		_, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{
				AppID: app.AppID,
				PlaceholderValues: map[string]string{
					"MEDIA_PATH": filepath.Join(t.TempDir(), "missing"),
				},
			},
			host,
			nil,
		)
		require.Error(t, err)
		assertUsageValidation(t, err)
	})

	t.Run("rejects path inside planned stack path", func(t *testing.T) {
		t.Parallel()

		tmp := t.TempDir()
		stackInternalPath := filepath.Join(tmp, "stacks", "jellyfin", "media")
		require.NoError(t, os.MkdirAll(stackInternalPath, 0o755))

		eng, _ := newTestEngine(
			t,
			core.WithCatalog(catalogFixtureFS(t, app)),
			core.WithStackBaseDir(filepath.Join(tmp, "stacks")),
		)
		_, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{
				AppID: app.AppID,
				PlaceholderValues: map[string]string{
					"MEDIA_PATH": stackInternalPath,
				},
			},
			host,
			nil,
		)
		require.Error(t, err)
		assertUsageValidation(t, err)
	})

	t.Run("accepts absolute existing path outside stack and resolves symlink", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		target := filepath.Join(root, "target")
		require.NoError(t, os.MkdirAll(target, 0o755))
		link := filepath.Join(root, "link")
		require.NoError(t, os.Symlink(target, link))

		plan, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{
				AppID: app.AppID,
				PlaceholderValues: map[string]string{
					"MEDIA_PATH": link,
				},
			},
			host,
			nil,
		)
		require.NoError(t, err)
		resolvedTarget, err := filepath.EvalSymlinks(target)
		require.NoError(t, err)
		assert.Equal(t, resolvedTarget, plan.ResolvedValues["MEDIA_PATH"])
	})
}

func TestInstallPlan_StackPathValidationRejectsTraversalAndUnsafeRoots(t *testing.T) {
	t.Parallel()

	app := appFixture("stack-path-app", freeLocalTCPPort(t))
	host := system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}

	cases := []struct {
		name      string
		stackPath string
	}{
		{name: "relative traversal", stackPath: "../stack"},
		{name: "home traversal", stackPath: "~/../stack"},
		{name: "absolute traversal", stackPath: "/tmp/../etc/wdm"},
		{name: "root", stackPath: "/"},
		{name: "system directory", stackPath: "/etc/wdm"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
			_, err := core.PlanInstallForTest(
				eng,
				t.Context(),
				types.InstallRequest{
					AppID:     app.AppID,
					StackPath: tc.stackPath,
				},
				host,
				nil,
			)
			require.Error(t, err)
			assertUsageValidation(t, err)
		})
	}
}

func TestInstallPlan_PortPlanning(t *testing.T) {
	t.Parallel()

	t.Run("binds 127.0.0.1 and defaults protocol to tcp", func(t *testing.T) {
		t.Parallel()

		app := appFixture("ports-app", freeLocalTCPPort(t))
		app.Ports = []catalog.Port{
			{Service: "web", Container: 8080, Host: freeLocalTCPPort(t), Protocol: ""},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
		plan, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{AppID: app.AppID},
			system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
			nil,
		)
		require.NoError(t, err)
		require.Len(t, plan.LocalPorts, 1)
		assert.Equal(t, "127.0.0.1", plan.LocalPorts[0].HostIP)
		assert.Equal(t, "tcp", plan.LocalPorts[0].Protocol)
	})

	t.Run("rejects duplicate host/protocol pairs", func(t *testing.T) {
		t.Parallel()

		port := freeLocalTCPPort(t)
		app := appFixture("duplicate-ports", port)
		app.Ports = []catalog.Port{
			{Service: "web", Container: 8080, Host: port, Protocol: "tcp"},
			{Service: "api", Container: 9090, Host: port, Protocol: "tcp"},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
		_, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{AppID: app.AppID},
			system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
			nil,
		)
		require.Error(t, err)
		assertVerificationFailed(t, err)
	})

	t.Run("rejects occupied ports", func(t *testing.T) {
		t.Parallel()

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		hostPort := ln.Addr().(*net.TCPAddr).Port
		app := appFixture("occupied-port", hostPort)
		app.Ports = []catalog.Port{
			{Service: "web", Container: 8080, Host: hostPort, Protocol: "tcp"},
		}
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

		_, err = core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{AppID: app.AppID},
			system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
			nil,
		)
		require.Error(t, err)
		assertUsageValidation(t, err)
		assert.Contains(t, err.Error(), strconv.Itoa(hostPort))
	})
}

func TestInstallPlan_ResourcePlanningAndEnvProjection(t *testing.T) {
	t.Parallel()

	newEngine := func(t *testing.T, app catalog.App) *core.Engine {
		t.Helper()
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))
		return eng
	}

	baseApp := appFixture("resource-app", freeLocalTCPPort(t))
	baseApp.Ports = []catalog.Port{}
	baseApp.Resources = []catalog.ResourceProfile{
		{
			Service: "app",
			Memory: catalog.MemoryBand{
				Min:         "1g",
				Recommended: "2g",
				Max:         "3g",
			},
			CPUs: catalog.CPUBand{
				Min:         "1.0",
				Recommended: "2.0",
				Max:         "3.0",
			},
			PIDs: catalog.PIDsBand{
				Default: 100,
				Max:     200,
			},
			AllowOverride: true,
		},
	}

	resourceAppWith := func(mutate func(*catalog.App)) catalog.App {
		app := baseApp
		app.Resources = append([]catalog.ResourceProfile(nil), baseApp.Resources...)
		mutate(&app)
		return app
	}

	t.Run("selects recommended and populates env keys", func(t *testing.T) {
		t.Parallel()

		eng := newEngine(t, baseApp)
		plan, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{AppID: baseApp.AppID},
			system.HostResources{CPUCores: 4, TotalMemoryBytes: 6 * gibibyte},
			nil,
		)
		require.NoError(t, err)
		assert.Equal(t, "2g", plan.ResolvedValues["MEMORY_LIMIT_APP"])
		assert.Equal(t, "2.0", plan.ResolvedValues["CPUS_LIMIT_APP"])
		assert.Equal(t, "100", plan.ResolvedValues["PIDS_LIMIT_APP"])
	})

	t.Run("falls back to min with progress event", func(t *testing.T) {
		t.Parallel()

		eng := newEngine(t, baseApp)
		var steps []string
		plan, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{AppID: baseApp.AppID},
			system.HostResources{CPUCores: 1, TotalMemoryBytes: 2 * gibibyte},
			func(step string, _ float64, _ string) { steps = append(steps, step) },
		)
		require.NoError(t, err)
		assert.Equal(t, "1g", plan.ResolvedValues["MEMORY_LIMIT_APP"])
		assert.Equal(t, "1.0", plan.ResolvedValues["CPUS_LIMIT_APP"])
		assert.Contains(t, steps, types.StepInstallResourceDegraded)
	})

	t.Run("uses min even below minimum guidance", func(t *testing.T) {
		t.Parallel()

		eng := newEngine(t, baseApp)
		var steps []string
		plan, err := core.PlanInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{AppID: baseApp.AppID},
			system.HostResources{CPUCores: 1, TotalMemoryBytes: gibibyte + (gibibyte / 2)},
			func(step string, _ float64, _ string) { steps = append(steps, step) },
		)
		require.NoError(t, err)
		assert.Equal(t, "1g", plan.ResolvedValues["MEMORY_LIMIT_APP"])
		assert.Equal(t, "1.0", plan.ResolvedValues["CPUS_LIMIT_APP"])
		assert.Contains(t, steps, types.StepInstallResourceDegraded)
	})

	t.Run("rejects invalid host resources", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name string
			host system.HostResources
		}{
			{name: "zero cpu", host: system.HostResources{CPUCores: 0, TotalMemoryBytes: 8 * gibibyte}},
			{name: "zero memory", host: system.HostResources{CPUCores: 4, TotalMemoryBytes: 0}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				eng := newEngine(t, baseApp)
				_, err := core.PlanInstallForTest(
					eng,
					t.Context(),
					types.InstallRequest{AppID: baseApp.AppID},
					tt.host,
					nil,
				)
				require.Error(t, err)
				assertUsageValidation(t, err)
				assert.Contains(t, err.Error(), "host resources could not be detected")
			})
		}
	})

	t.Run("rejects invalid catalog resources", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name   string
			mutate func(*catalog.App)
			host   system.HostResources
			want   string
		}{
			{
				name: "bad recommended memory",
				mutate: func(app *catalog.App) {
					app.Resources[0].Memory.Recommended = "two"
				},
				host: system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
				want: "schema validation",
			},
			{
				name: "bad recommended cpu",
				mutate: func(app *catalog.App) {
					app.Resources[0].CPUs.Recommended = "many"
				},
				host: system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
				want: "schema validation",
			},
			{
				name: "bad min memory on degraded path",
				mutate: func(app *catalog.App) {
					app.Resources[0].Memory.Min = "one"
				},
				host: system.HostResources{CPUCores: 1, TotalMemoryBytes: 2 * gibibyte},
				want: "schema validation",
			},
			{
				name: "bad min cpu on degraded path",
				mutate: func(app *catalog.App) {
					app.Resources[0].CPUs.Min = "few"
				},
				host: system.HostResources{CPUCores: 1, TotalMemoryBytes: 2 * gibibyte},
				want: "schema validation",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				app := resourceAppWith(tt.mutate)
				eng := newEngine(t, app)
				_, err := core.PlanInstallForTest(
					eng,
					t.Context(),
					types.InstallRequest{AppID: app.AppID},
					tt.host,
					nil,
				)
				require.Error(t, err)
				assertVerificationFailed(t, err)
				assert.Contains(t, err.Error(), tt.want)
			})
		}
	})

	t.Run("rejects invalid overrides", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			app      catalog.App
			override types.ResourceOverride
			want     string
		}{
			{
				name:     "unknown service",
				app:      baseApp,
				override: types.ResourceOverride{Service: "missing", Memory: "512m"},
				want:     "resource override targets an unknown service",
			},
			{
				name: "disallowed service",
				app: resourceAppWith(func(app *catalog.App) {
					app.Resources[0].AllowOverride = false
				}),
				override: types.ResourceOverride{Service: "app", Memory: "512m"},
				want:     "resource override is not allowed for this service",
			},
			{
				name:     "invalid memory",
				app:      baseApp,
				override: types.ResourceOverride{Service: "app", Memory: "large"},
				want:     "memory override is invalid",
			},
			{
				name:     "memory out of range",
				app:      baseApp,
				override: types.ResourceOverride{Service: "app", Memory: "4g"},
				want:     "memory override is out of range",
			},
			{
				name:     "invalid cpu",
				app:      baseApp,
				override: types.ResourceOverride{Service: "app", CPUs: "fast"},
				want:     "cpu override is invalid",
			},
			{
				name:     "cpu out of range",
				app:      baseApp,
				override: types.ResourceOverride{Service: "app", CPUs: "4.0"},
				want:     "cpu override is out of range",
			},
			{
				name:     "pids out of range",
				app:      baseApp,
				override: types.ResourceOverride{Service: "app", PIDs: 201},
				want:     "pids override is out of range",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				eng := newEngine(t, tt.app)
				_, err := core.PlanInstallForTest(
					eng,
					t.Context(),
					types.InstallRequest{
						AppID: tt.app.AppID,
						ResourceOverrides: []types.ResourceOverride{
							tt.override,
						},
					},
					system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
					nil,
				)
				require.Error(t, err)
				assertUsageValidation(t, err)
				assert.Contains(t, err.Error(), tt.want)
			})
		}
	})
}

func TestInstallPlan_SecretPlaceholdersAreCollectedButNotResolved(t *testing.T) {
	t.Parallel()

	app := appFixture("secret-app", freeLocalTCPPort(t))
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url"},
		{Name: "SITE_NAME", Type: "string", Required: true},
	}
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFixtureFS(t, app)))

	plan, err := core.PlanInstallForTest(
		eng,
		t.Context(),
		types.InstallRequest{
			AppID: app.AppID,
			PlaceholderValues: map[string]string{
				"SITE_NAME": "My Site",
			},
		},
		system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
		nil,
	)
	require.NoError(t, err)

	assert.NotContains(t, plan.ResolvedValues, "DB_PASSWORD")
	assert.Contains(t, plan.GeneratedFields, "DB_PASSWORD")
	assert.Equal(t, "My Site", plan.ResolvedValues["SITE_NAME"])
}

func TestInstallRender_GeneratesSecretsAndRendersStackInMemory(t *testing.T) {
	t.Parallel()

	app := appFixture("render-app", freeLocalTCPPort(t))
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/render-app/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/render-app/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url"},
		{Name: "API_TOKEN", Type: "secret", Required: true, Encoding: "hex"},
		{Name: "SITE_NAME", Type: "string", Required: true},
	}
	app.AdditionalFiles = []catalog.AdditionalFile{
		{
			Src:   "init-data.sh",
			Dest:  "init-data.sh",
			Mode:  "0755",
			Mount: "./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro",
		},
	}

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/render-app/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - ./init-data.sh:/docker-entrypoint-initdb.d/init-data.sh:ro
    environment:
      DB_PASSWORD: ${DB_PASSWORD}
      API_TOKEN: ${API_TOKEN}
      SITE_NAME: ${SITE_NAME}
`,
		"templates/render-app/.env.tmpl":    "DB_PASSWORD={{ .DB_PASSWORD }}\nAPI_TOKEN={{ .API_TOKEN }}\nPUID={{ .UID }}\nSITE_NAME={{ .SITE_NAME }}\n",
		"templates/render-app/init-data.sh": "echo {{ .SITE_NAME }}\n",
	}, app)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, func(enc security.Encoding) (string, error) {
		switch enc {
		case security.EncodingBase64URL:
			return "generated-base64url-secret", nil
		case security.EncodingHex:
			return "abcdef0123456789", nil
		default:
			return "", fmt.Errorf("unexpected encoding %q", enc)
		}
	})

	var steps []string
	snapshot, err := core.RenderInstallForTest(
		eng,
		t.Context(),
		types.InstallRequest{
			AppID: app.AppID,
			PlaceholderValues: map[string]string{
				"SITE_NAME": "WDM",
			},
		},
		system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
		func(step string, _ float64, _ string) { steps = append(steps, step) },
	)
	require.NoError(t, err)

	assert.Contains(t, steps, types.StepInstallPlanning)
	assert.Contains(t, steps, types.StepInstallRender)
	assert.Equal(t, []string{"DB_PASSWORD", "API_TOKEN"}, snapshot.GeneratedFields)
	assert.Equal(t, "generated-base64url-secret", snapshot.ResolvedValues["DB_PASSWORD"])
	assert.Equal(t, "abcdef0123456789", snapshot.ResolvedValues["API_TOKEN"])
	assert.Contains(t, string(snapshot.EnvBytes), "DB_PASSWORD=generated-base64url-secret")
	assert.Contains(t, string(snapshot.EnvBytes), "API_TOKEN=abcdef0123456789")
	assert.Contains(t, string(snapshot.EnvBytes), "PUID="+strconv.Itoa(os.Getuid()))
	assert.NotContains(t, string(snapshot.ComposeBytes), "generated-base64url-secret")
	assert.NotContains(t, string(snapshot.ComposeBytes), "abcdef0123456789")
	assert.Equal(t, "true", snapshot.ServiceLabels["app"]["wdm.managed"])
	assert.Equal(t, app.AppID, snapshot.ServiceLabels["app"]["wdm.app"])
	require.Len(t, snapshot.AdditionalFiles, 1)
	assert.Equal(t, "init-data.sh", snapshot.AdditionalFiles[0].Dest)
	assert.Equal(t, "0755", snapshot.AdditionalFiles[0].Mode)
	assert.Equal(t, []byte("echo WDM\n"), snapshot.AdditionalFiles[0].Bytes)
}

func TestInstallRender_RedactsGeneratedSecretWhenComposeInlinesIt(t *testing.T) {
	t.Parallel()

	const generated = "known-generated-secret"

	app := appFixture("unsafe-render-app", freeLocalTCPPort(t))
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/unsafe/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/unsafe/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url"},
	}

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/unsafe/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
    environment:
      DB_PASSWORD: "{{ .DB_PASSWORD }}"
`,
		"templates/unsafe/.env.tmpl": "DB_PASSWORD={{ .DB_PASSWORD }}\n",
	}, app)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		return generated, nil
	})

	_, err := core.RenderInstallForTest(
		eng,
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
		nil,
	)
	require.Error(t, err)
	assertVerificationFailed(t, err)
	assert.NotContains(t, err.Error(), generated)
	assertErrorChainDoesNotContain(t, err, generated)
	assert.Contains(t, err.Error(), security.RedactedPlaceholder)
}

func TestInstallRender_RejectsGeneratedSecretInGuidanceTemplate(t *testing.T) {
	t.Parallel()

	const generated = "guidance-generated-secret"

	app := appFixture("unsafe-guidance-app", freeLocalTCPPort(t))
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/unsafe-guidance/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/unsafe-guidance/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url"},
	}
	// A catalog template that would surface a generated secret on the
	// install-finish screen must fail closed before any sink sees it.
	app.LocalTargetURLTemplate = "http://127.0.0.1/?token={{ .DB_PASSWORD }}"

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/unsafe-guidance/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
`,
		"templates/unsafe-guidance/.env.tmpl": "DB_PASSWORD={{ .DB_PASSWORD }}\n",
	}, app)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		return generated, nil
	})

	_, err := core.RenderInstallForTest(
		eng,
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
		nil,
	)
	require.Error(t, err)
	assertVerificationFailed(t, err)
	assert.NotContains(t, err.Error(), generated)
	assertErrorChainDoesNotContain(t, err, generated)
}

// argon2idFixturePHC is the synthetic PHC the argon2id install tests pin
// through the generator seam. It carries a literal `$` in every PHC
// segment so the $$-escaping is observable in the rendered .env, and a
// b64 alphabet character ("/") in the hash field that the escaping must
// leave untouched.
const argon2idFixturePHC = "$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0c2E$aGFzaC9oYXNoaGFzaA"

// argon2idFixturePlaintext is the synthetic one-time plaintext the seam
// surfaces. It is intentionally distinctive so leak assertions can search
// for it across every sink.
const argon2idFixturePlaintext = "argon2id-one-time-plaintext-value"

// argon2idInstallApp builds a single-secret install fixture whose only
// placeholder is an argon2id secret, plus its catalog FS. The compose
// template never references the secret (it is a hash bound for .env only),
// so the rendered Compose stays clean.
func argon2idInstallApp(t *testing.T) (catalog.App, fs.FS) {
	t.Helper()

	regenerableFalse := false
	app := appFixture("argon2id-app", freeLocalTCPPort(t))
	app.Name = "Vaultwarden"
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/argon2id/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/argon2id/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "ADMIN_TOKEN", Type: "secret", Required: true, Encoding: "argon2id", Regenerable: &regenerableFalse},
	}

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/argon2id/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
    env_file:
      - .env
`,
		"templates/argon2id/.env.tmpl": "ADMIN_TOKEN={{ .ADMIN_TOKEN }}\n",
	}, app)
	return app, catalogFS
}

// pinArgon2idGenerator wires the deterministic argon2id credential seam on
// eng so the surfaced plaintext and persisted PHC are fixed for assertions.
func pinArgon2idGenerator(t *testing.T, eng *core.Engine) {
	t.Helper()
	core.SetInstallArgon2idGeneratorForTest(eng, func() (string, string, error) {
		return argon2idFixturePlaintext, argon2idFixturePHC, nil
	})
}

// TestInstallRender_Argon2idEscapesPHCInEnvAndSurfacesPlaintextOnce is the
// core foundation assertion: the rendered .env carries the $$-doubled PHC
// (so Compose --env-file interpolation reconstructs the canonical PHC), the
// one-time plaintext rides on the guidance exactly once, and the plaintext
// never lands in the .env, resolved values, compose, or the leak-check
// guidance text.
func TestInstallRender_Argon2idEscapesPHCInEnvAndSurfacesPlaintextOnce(t *testing.T) {
	t.Parallel()

	app, catalogFS := argon2idInstallApp(t)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	pinArgon2idGenerator(t, eng)

	snapshot, err := core.RenderInstallForTest(
		eng,
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
		nil,
	)
	require.NoError(t, err)

	escapedPHC := strings.ReplaceAll(argon2idFixturePHC, "$", "$$")
	// The .env carries the $$-doubled PHC verbatim, never the raw single-$ PHC.
	assert.Contains(t, string(snapshot.EnvBytes), "ADMIN_TOKEN="+escapedPHC,
		"the .env line must carry the $$-escaped PHC so Compose interpolation reconstructs it")
	assert.NotContains(t, string(snapshot.EnvBytes), "ADMIN_TOKEN="+argon2idFixturePHC+"\n",
		"the .env must not carry the un-escaped single-$ PHC")
	// The resolved value (which the renderer consumes) is the escaped PHC,
	// never the plaintext.
	assert.Equal(t, escapedPHC, snapshot.ResolvedValues["ADMIN_TOKEN"])

	// The plaintext is surfaced exactly once on the guidance and nowhere else.
	require.NotNil(t, snapshot.Guidance)
	require.Len(t, snapshot.Guidance.GeneratedCredentials, 1)
	cred := snapshot.Guidance.GeneratedCredentials[0]
	assert.Equal(t, argon2idFixturePlaintext, cred.Value)
	assert.Equal(t, "Vaultwarden ADMIN_TOKEN", cred.Label)
	assert.NotEmpty(t, cred.Note)

	// The plaintext must never appear in any persisted artifact.
	assert.NotContains(t, string(snapshot.EnvBytes), argon2idFixturePlaintext)
	assert.NotContains(t, string(snapshot.ComposeBytes), argon2idFixturePlaintext)
	for _, v := range snapshot.ResolvedValues {
		assert.NotContains(t, v, argon2idFixturePlaintext)
	}
}

// TestInstallRender_Argon2idPlaintextExcludedFromJSON pins PRD §24: the
// surfaced plaintext is reachable to in-process consumers via the struct
// but is provably excluded from the JSON envelope the --json path emits.
func TestInstallRender_Argon2idPlaintextExcludedFromJSON(t *testing.T) {
	t.Parallel()

	app, catalogFS := argon2idInstallApp(t)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})
	pinArgon2idGenerator(t, eng)
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(&fakeDockerClient{}))

	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	// In-process: the plaintext is reachable on the struct (the finish
	// screen reads it here).
	require.NotNil(t, res.PostInstallGuidance)
	require.Len(t, res.PostInstallGuidance.GeneratedCredentials, 1)
	assert.Equal(t, argon2idFixturePlaintext, res.PostInstallGuidance.GeneratedCredentials[0].Value)

	// JSON: the plaintext (and the whole GeneratedCredentials field) is
	// excluded by the json:"-" tag — the §24 guarantee the --json path relies on.
	jsonBytes, err := json.Marshal(res)
	require.NoError(t, err)
	assert.NotContains(t, string(jsonBytes), argon2idFixturePlaintext,
		"the one-time plaintext must never appear in JSON output (PRD §24)")
	assert.NotContains(t, string(jsonBytes), "GeneratedCredentials",
		"the GeneratedCredentials field must be dropped from JSON entirely")

	// The persisted .env on disk carries only the escaped PHC, never the plaintext.
	stackPath := filepath.Join(filepath.Dir(stateDir), "stacks", app.AppID)
	envBytes, err := os.ReadFile(filepath.Join(stackPath, ".env"))
	require.NoError(t, err)
	assert.NotContains(t, string(envBytes), argon2idFixturePlaintext)
	assert.Contains(t, string(envBytes), strings.ReplaceAll(argon2idFixturePHC, "$", "$$"))
}

// TestInstallRender_Argon2idPHCRejectedInGuidanceLeakCheck asserts the PHC
// (a generatedValues member) is treated as a generated secret by the
// non-secret leak check: a catalog guidance template that would surface it
// fails closed, and the redactor scrubs the escaped PHC from the error.
func TestInstallRender_Argon2idPHCRejectedInGuidanceLeakCheck(t *testing.T) {
	t.Parallel()

	regenerableFalse := false
	app := appFixture("argon2id-leak-app", freeLocalTCPPort(t))
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/argon2id-leak/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/argon2id-leak/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "ADMIN_TOKEN", Type: "secret", Required: true, Encoding: "argon2id", Regenerable: &regenerableFalse},
	}
	// A guidance template that splices the PHC onto the finish screen must
	// fail closed before any sink sees it.
	app.LocalTargetURLTemplate = "http://127.0.0.1/?h={{ .ADMIN_TOKEN }}"

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/argon2id-leak/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
`,
		"templates/argon2id-leak/.env.tmpl": "ADMIN_TOKEN={{ .ADMIN_TOKEN }}\n",
	}, app)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	pinArgon2idGenerator(t, eng)

	_, err := core.RenderInstallForTest(
		eng,
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
		nil,
	)
	require.Error(t, err)
	assertVerificationFailed(t, err)

	escapedPHC := strings.ReplaceAll(argon2idFixturePHC, "$", "$$")
	assert.NotContains(t, err.Error(), escapedPHC, "the PHC must be redacted from the error")
	assert.NotContains(t, err.Error(), argon2idFixturePlaintext, "the plaintext must never reach an error")
	assertErrorChainDoesNotContain(t, err, escapedPHC)
	assertErrorChainDoesNotContain(t, err, argon2idFixturePlaintext)
	assert.Contains(t, err.Error(), security.RedactedPlaceholder)
}

func TestInstall_WritesRenderedFilesBeforeDeployment(t *testing.T) {
	t.Parallel()

	app := appFixture("install-write-app", freeLocalTCPPort(t))
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/install-write-app/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/install-write-app/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url"},
		{Name: "SITE_NAME", Type: "string", Required: true},
	}
	app.AdditionalFiles = []catalog.AdditionalFile{
		{
			Src:  "scripts/init-data.sh.tmpl",
			Dest: "nested/init-data.sh",
			Mode: "0755",
		},
	}

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/install-write-app/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
`,
		"templates/install-write-app/.env.tmpl":                 "DB_PASSWORD={{ .DB_PASSWORD }}\nSITE_NAME={{ .SITE_NAME }}\n",
		"templates/install-write-app/scripts/init-data.sh.tmpl": "echo {{ .SITE_NAME }}\n",
	}, app)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		return "install-secret", nil
	})
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(&fakeDockerClient{}))

	req := types.InstallRequest{
		AppID: app.AppID,
		PlaceholderValues: map[string]string{
			"SITE_NAME": "WDM",
		},
	}
	rendered, err := core.RenderInstallForTest(
		eng,
		t.Context(),
		req,
		system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
		nil,
	)
	require.NoError(t, err)

	var steps []string
	res, err := eng.Install(
		t.Context(),
		req,
		func(step string, _ float64, _ string) { steps = append(steps, step) },
		&fakeConfirmer{},
	)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Contains(t, steps, types.StepInstallRender)
	assert.Contains(t, steps, types.StepInstallWriteFiles)

	stackPath := filepath.Join(filepath.Dir(stateDir), "stacks", app.AppID)
	require.Equal(t, stackPath, rendered.StackPath)
	require.FileExists(t, filepath.Join(stackPath, ".wdm.lock"))

	composePath := filepath.Join(stackPath, "docker-compose.yml")
	envPath := filepath.Join(stackPath, ".env")
	additionalPath := filepath.Join(stackPath, "nested", "init-data.sh")

	composeBytes, err := os.ReadFile(composePath)
	require.NoError(t, err)
	envBytes, err := os.ReadFile(envPath)
	require.NoError(t, err)
	additionalBytes, err := os.ReadFile(additionalPath)
	require.NoError(t, err)

	assert.Equal(t, rendered.ComposeBytes, composeBytes)
	assert.Equal(t, rendered.EnvBytes, envBytes)
	require.Len(t, rendered.AdditionalFiles, 1)
	assert.Equal(t, rendered.AdditionalFiles[0].Bytes, additionalBytes)
	assert.Equal(t, os.FileMode(0o644), fileModePerm(t, composePath))
	assert.NotEqual(t, security.SecretFileMode, fileModePerm(t, composePath))
	assert.Equal(t, security.SecretFileMode, fileModePerm(t, envPath))
	assert.Equal(t, os.FileMode(0o755), fileModePerm(t, additionalPath))
	assert.NotContains(t, string(composeBytes), "install-secret")
	assert.NotContains(t, string(additionalBytes), "install-secret")
}

func TestInstall_ValidatesComposeConfirmsAndPreCreatesNetworksBeforeDeployment(t *testing.T) {
	t.Parallel()

	const generatedSecret = "deploy-generated-secret"

	port := freeLocalTCPPort(t)
	app := appFixture("deploy-app", port)
	app.ComposeTemplate = "templates/deploy-app/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/deploy-app/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url"},
	}
	app.Networks = []catalog.Network{
		{Name: "wdm_front", Internal: false},
		{Name: "wdm_back", Internal: true},
	}

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/deploy-app/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
`,
		"templates/deploy-app/.env.tmpl": "DB_PASSWORD={{ .DB_PASSWORD }}\n",
	}, app)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		return generatedSecret, nil
	})

	stackPath := filepath.Join(filepath.Dir(stateDir), "stacks", app.AppID)
	composePath := filepath.Join(stackPath, "docker-compose.yml")
	fake := &fakeDockerClient{}
	fake.runFn = func(call int, _ docker.Invocation) (docker.CommandResult, error) {
		switch call {
		case 1:
			// Compose validation must run against a tempdir copy before
			// any stack byte is exposed (protocol step 4 pre-exposure).
			assert.NoFileExists(t, composePath)
			return docker.CommandResult{}, nil
		case 2:
			return missingNetworkResult("wdm_front")
		case 4:
			return missingNetworkResult("wdm_back")
		default:
			return docker.CommandResult{}, nil
		}
	}

	var capturedRedactor security.Redactor
	core.SetInstallDockerClientFactoryForTest(eng, func(redactor security.Redactor) (docker.Client, error) {
		capturedRedactor = redactor
		return fake, nil
	})

	confirmer := &fakeConfirmer{}
	var steps []string
	res, err := eng.Install(
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		func(step string, _ float64, _ string) { steps = append(steps, step) },
		confirmer,
	)
	require.NoError(t, err)
	require.NotNil(t, res)

	validateIdx := stepIndex(t, steps, types.StepInstallComposeValidate)
	writeIdx := stepIndex(t, steps, types.StepInstallWriteFiles)
	confirmIdx := stepIndex(t, steps, types.StepInstallConfirm)
	networkIdx := stepIndex(t, steps, types.StepInstallNetworkCreate)
	deployIdx := stepIndex(t, steps, types.StepInstallDeploy)
	lockIdx := stepIndex(t, steps, types.StepInstallLockUpdate)
	statusIdx := stepIndex(t, steps, types.StepInstallStatus)
	assert.Less(t, validateIdx, writeIdx)
	assert.Less(t, writeIdx, confirmIdx)
	assert.Less(t, confirmIdx, networkIdx)
	assert.Less(t, networkIdx, deployIdx)
	assert.Less(t, deployIdx, lockIdx)
	assert.Less(t, lockIdx, statusIdx)

	// One compose-config validation, inspect+create per network, one
	// up -d, one image-digest inspect, one project-container list.
	assert.Equal(t, 8, fake.calls)

	require.Len(t, confirmer.calls, 1)
	confirmation := confirmer.calls[0]
	assert.Equal(t, "install_deploy", confirmation.Kind)
	assert.Contains(t, confirmation.Title, app.AppID)
	assert.Contains(t, confirmation.Message, stackPath)
	assert.Contains(t, confirmation.Message, strconv.Itoa(port))
	assert.Contains(t, confirmation.Message, "wdm_front")
	assert.Contains(t, confirmation.Message, "wdm_back (internal)")
	assert.NotContains(t, confirmation.Message, generatedSecret)

	require.FileExists(t, composePath)
	require.NotNil(t, capturedRedactor)
	assert.Equal(t, security.RedactedPlaceholder, capturedRedactor.Redact(generatedSecret))
}

func TestInstall_ComposeValidationFailureStopsBeforeFileWrites(t *testing.T) {
	t.Parallel()

	app := appFixture("compose-invalid-app", freeLocalTCPPort(t))
	eng, stackPath := newInstallDeployTestEngine(t, app)

	composeErr := errors.New("compose config rejected")
	fake := &fakeDockerClient{
		runFn: func(int, docker.Invocation) (docker.CommandResult, error) {
			return docker.CommandResult{}, composeErr
		},
	}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	confirmer := &fakeConfirmer{}
	var steps []string
	res, err := eng.Install(
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		func(step string, _ float64, _ string) { steps = append(steps, step) },
		confirmer,
	)
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, composeErr)
	assert.NotErrorIs(t, err, types.ErrNotImplemented)

	assert.Equal(t, 1, fake.calls)
	assert.Empty(t, confirmer.calls)
	assert.Contains(t, steps, types.StepInstallComposeValidate)
	assert.NotContains(t, steps, types.StepInstallWriteFiles)
	assert.NotContains(t, steps, types.StepInstallConfirm)
	assert.NotContains(t, steps, types.StepInstallNetworkCreate)
	assert.NoDirExists(t, stackPath)
}

func TestInstall_NilConfirmerRefusesPastConfirmationStep(t *testing.T) {
	t.Parallel()

	app := appFixture("nil-confirmer-app", freeLocalTCPPort(t))
	app.Networks = []catalog.Network{{Name: "wdm_front", Internal: false}}
	eng, stackPath := newInstallDeployTestEngine(t, app)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	var steps []string
	res, err := eng.Install(
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		func(step string, _ float64, _ string) { steps = append(steps, step) },
		nil,
	)
	require.Error(t, err)
	assert.Nil(t, res)
	assertUsageValidation(t, err)
	assert.NotErrorIs(t, err, types.ErrNotImplemented)

	// Refusal happens at the confirmation step after exposure, no
	// deployment-shaped Docker work runs, and the protocol step 7
	// fresh-install sad path removes the partial files.
	assert.Equal(t, 1, fake.calls)
	assert.Contains(t, steps, types.StepInstallWriteFiles)
	assert.NotContains(t, steps, types.StepInstallNetworkCreate)
	assert.NoDirExists(t, stackPath)
}

func TestInstall_ConfirmerDeclineCancelsBeforeNetworkCreation(t *testing.T) {
	t.Parallel()

	app := appFixture("declined-app", freeLocalTCPPort(t))
	app.Networks = []catalog.Network{{Name: "wdm_front", Internal: false}}
	eng, stackPath := newInstallDeployTestEngine(t, app)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			return false, nil
		},
	}
	var steps []string
	res, err := eng.Install(
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		func(step string, _ float64, _ string) { steps = append(steps, step) },
		confirmer,
	)
	require.Error(t, err)
	assert.Nil(t, res)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeUserCanceled, typedErr.Code)
	assert.Equal(t, 1, fake.calls)
	assert.Contains(t, steps, types.StepInstallConfirm)
	assert.NotContains(t, steps, types.StepInstallNetworkCreate)

	// A decline arms no Docker rollback: no network was created and no
	// deploy was attempted, so the sad path runs file cleanup only —
	// never a compose down, volume listing, or resource removal.
	assert.NotContains(t, fake.invocationTypes, "docker.composeDownInvocation")
	assert.NotContains(t, fake.invocationTypes, "docker.projectVolumeListInvocation")
	assert.NotContains(t, fake.invocationTypes, "docker.removeNamedVolumeInvocation")
	assert.NotContains(t, fake.invocationTypes, "docker.removeNetworkInvocation")

	// Decline falls through to the protocol step 7 fresh-install sad
	// path: written files, the empty.wdm.lock, and the created stack
	// directory are all removed.
	assert.NoFileExists(t, filepath.Join(stackPath, "docker-compose.yml"))
	assert.NoFileExists(t, filepath.Join(stackPath, ".env"))
	assert.NoFileExists(t, filepath.Join(stackPath, ".wdm.lock"))
	assert.NoDirExists(t, stackPath)
}

func TestInstall_ConfirmerErrorAbortsBeforeNetworkCreation(t *testing.T) {
	t.Parallel()

	app := appFixture("confirm-error-app", freeLocalTCPPort(t))
	app.Networks = []catalog.Network{{Name: "wdm_front", Internal: false}}
	eng, stackPath := newInstallDeployTestEngine(t, app)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	confirmErr := errors.New("confirm prompt failed")
	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			return true, confirmErr
		},
	}
	var steps []string
	res, err := eng.Install(
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		func(step string, _ float64, _ string) { steps = append(steps, step) },
		confirmer,
	)
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, confirmErr)
	assert.Equal(t, 1, fake.calls)
	assert.NotContains(t, steps, types.StepInstallNetworkCreate)
	assert.NoDirExists(t, stackPath)
}

func TestInstall_NetworkInternalFlagDriftFailsClosed(t *testing.T) {
	t.Parallel()

	app := appFixture("network-drift-app", freeLocalTCPPort(t))
	app.Networks = []catalog.Network{{Name: "wdm_front", Internal: false}}
	eng, stackPath := newInstallDeployTestEngine(t, app)

	fake := &fakeDockerClient{
		runFn: func(call int, _ docker.Invocation) (docker.CommandResult, error) {
			if call == 2 {
				return docker.CommandResult{Stdout: "true\n"}, nil
			}
			return docker.CommandResult{}, nil
		},
	}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.NotErrorIs(t, err, types.ErrNotImplemented)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
	assert.Equal(t, "network wdm_front exists with mismatched internal flag", typedErr.Hint)
	assert.Equal(t, 2, fake.calls)
	assert.NoDirExists(t, stackPath)
}

func TestInstall_NetworkCreationFailurePropagates(t *testing.T) {
	t.Parallel()

	app := appFixture("network-create-app", freeLocalTCPPort(t))
	app.Networks = []catalog.Network{{Name: "wdm_front", Internal: false}}
	eng, stackPath := newInstallDeployTestEngine(t, app)

	createErr := errors.New("network create denied")
	fake := &fakeDockerClient{
		runFn: func(call int, _ docker.Invocation) (docker.CommandResult, error) {
			switch call {
			case 2:
				return missingNetworkResult("wdm_front")
			case 3:
				return docker.CommandResult{}, createErr
			default:
				return docker.CommandResult{}, nil
			}
		},
	}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, createErr)
	assert.NotErrorIs(t, err, types.ErrNotImplemented)
	assert.Equal(t, 3, fake.calls)

	// Network creation failed before any network was created and before
	// any deploy: the rollback is unarmed, so no Docker resource is
	// removed (no compose down, no volume listing, no network removal).
	assert.NotContains(t, fake.invocationTypes, "docker.composeDownInvocation")
	assert.NotContains(t, fake.invocationTypes, "docker.projectVolumeListInvocation")
	assert.NotContains(t, fake.invocationTypes, "docker.removeNetworkInvocation")
	assert.NoDirExists(t, stackPath)
}

func TestInstall_PortRecheckConflictBeforeDeploymentFailsClosed(t *testing.T) {
	t.Parallel()

	port := freeLocalTCPPort(t)
	app := appFixture("port-recheck-app", port)
	eng, stackPath := newInstallDeployTestEngine(t, app)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	// The first port check passed at planning time; occupying the port
	// inside the confirm callback proves the second, pre-deployment
	// check fires.
	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			require.NoError(t, err)
			t.Cleanup(func() { _ = ln.Close() })
			return true, nil
		},
	}
	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	assert.NotErrorIs(t, err, types.ErrNotImplemented)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
	assert.Contains(t, typedErr.Hint, strconv.Itoa(port))
	assert.Equal(t, 1, fake.calls)
	assert.NoDirExists(t, stackPath)
}

func TestInstall_ContextCancellationAfterConfirmStopsNetworkCreation(t *testing.T) {
	t.Parallel()

	app := appFixture("cancel-app", freeLocalTCPPort(t))
	app.Networks = []catalog.Network{{Name: "wdm_front", Internal: false}}
	eng, stackPath := newInstallDeployTestEngine(t, app)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			cancel()
			return true, nil
		},
	}
	res, err := eng.Install(ctx, types.InstallRequest{AppID: app.AppID}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, fake.calls)
	assert.NoDirExists(t, stackPath)
}

func TestInstall_DockerClientFactoryFailuresFailClosedBeforeWrites(t *testing.T) {
	t.Parallel()

	factoryErr := errors.New("factory exploded")
	tests := []struct {
		name    string
		factory func(security.Redactor) (docker.Client, error)
		check   func(t *testing.T, err error)
	}{
		{
			name:    "nil factory",
			factory: nil,
			check: func(t *testing.T, err error) {
				t.Helper()
				assertUsageValidation(t, err)
			},
		},
		{
			name: "factory error",
			factory: func(security.Redactor) (docker.Client, error) {
				return nil, factoryErr
			},
			check: func(t *testing.T, err error) {
				t.Helper()
				require.ErrorIs(t, err, factoryErr)
			},
		},
		{
			name: "nil client",
			factory: func(security.Redactor) (docker.Client, error) {
				return nil, nil
			},
			check: func(t *testing.T, err error) {
				t.Helper()
				assertUsageValidation(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			app := appFixture("factory-app", freeLocalTCPPort(t))
			eng, stackPath := newInstallDeployTestEngine(t, app)
			core.SetInstallDockerClientFactoryForTest(eng, tt.factory)

			res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
			require.Error(t, err)
			assert.Nil(t, res)
			tt.check(t, err)
			assert.NoDirExists(t, stackPath)
		})
	}
}

func TestWriteInstallFilesForTest_RejectsEscapingAdditionalFileDest(t *testing.T) {
	t.Parallel()

	root := coreTestTempDir(t)
	stackPath := filepath.Join(root, "stacks", "escape-app")
	escapePath := filepath.Join(root, "stacks", "escape.sh")

	err := core.WriteInstallFilesForTest(
		t.Context(),
		stackPath,
		render.RenderedStack{
			ComposeBytes: []byte("services: {}\n"),
			EnvBytes:     []byte("TOKEN=secret\n"),
			AdditionalFiles: []render.RenderedFile{
				{
					Dest:  "../escape.sh",
					Mode:  "0755",
					Bytes: []byte("echo escaped\n"),
				},
			},
		},
		nil,
	)
	require.Error(t, err)
	assertVerificationFailed(t, err)
	assert.NoFileExists(t, escapePath)
}

func TestWriteInstallFilesForTest_RefusesExistingManagedStack(t *testing.T) {
	t.Parallel()

	root := coreTestTempDir(t)
	stackPath := filepath.Join(root, "stacks", "managed-app")
	require.NoError(t, os.MkdirAll(stackPath, 0o755))

	composePath := filepath.Join(stackPath, "docker-compose.yml")
	require.NoError(t, os.WriteFile(composePath, []byte("old\n"), 0o644))

	lock, err := state.AcquireStackLock(t.Context(), filepath.Join(stackPath, ".wdm.lock"))
	require.NoError(t, err)
	require.NoError(t, lock.Write(state.StackLock{
		SchemaVersion:   1,
		AppID:           "managed-app",
		TemplateName:    "managed-app",
		TemplateVersion: "2026.05.29",
		CatalogChannel:  "stable",
		CatalogVersion:  "2026.05.29",
		StackPath:       stackPath,
		ComposeProject:  "wdm-managed-app",
	}))
	require.NoError(t, lock.Release())

	err = core.WriteInstallFilesForTest(
		t.Context(),
		stackPath,
		render.RenderedStack{
			ComposeBytes: []byte("new\n"),
			EnvBytes:     []byte("TOKEN=secret\n"),
		},
		nil,
	)
	require.Error(t, err)
	assertUsageValidation(t, err)

	raw, err := os.ReadFile(composePath)
	require.NoError(t, err)
	assert.Equal(t, []byte("old\n"), raw)
}

func TestWriteInstallFilesForTest_RefusesExistingUnmanagedStack(t *testing.T) {
	t.Parallel()

	root := coreTestTempDir(t)
	stackPath := filepath.Join(root, "stacks", "unmanaged-app")
	require.NoError(t, os.MkdirAll(stackPath, 0o755))

	composePath := filepath.Join(stackPath, "docker-compose.yml")
	envPath := filepath.Join(stackPath, ".env")
	require.NoError(t, os.WriteFile(composePath, []byte("old compose\n"), 0o644))
	require.NoError(t, os.WriteFile(envPath, []byte("OLD_ENV=1\n"), 0o600))

	err := core.WriteInstallFilesForTest(
		t.Context(),
		stackPath,
		render.RenderedStack{
			ComposeBytes: []byte("new compose\n"),
			EnvBytes:     []byte("NEW_ENV=1\n"),
		},
		nil,
	)
	require.Error(t, err)
	assertUsageValidation(t, err)

	composeBytes, err := os.ReadFile(composePath)
	require.NoError(t, err)
	envBytes, err := os.ReadFile(envPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("old compose\n"), composeBytes)
	assert.Equal(t, []byte("OLD_ENV=1\n"), envBytes)
	assert.NoFileExists(t, filepath.Join(stackPath, ".wdm.lock"))
}

func TestWriteInstallFilesForTest_RefusesManagedStackWithoutChangingMode(t *testing.T) {
	t.Parallel()

	root := coreTestTempDir(t)
	stackPath := filepath.Join(root, "stacks", "managed-mode-app")
	require.NoError(t, os.MkdirAll(stackPath, 0o700))
	require.NoError(t, os.Chmod(stackPath, 0o700))

	lock, err := state.AcquireStackLock(t.Context(), filepath.Join(stackPath, ".wdm.lock"))
	require.NoError(t, err)
	require.NoError(t, lock.Write(state.StackLock{
		SchemaVersion:   1,
		AppID:           "managed-mode-app",
		TemplateName:    "managed-mode-app",
		TemplateVersion: "2026.05.29",
		CatalogChannel:  "stable",
		CatalogVersion:  "2026.05.29",
		StackPath:       stackPath,
		ComposeProject:  "wdm-managed-mode-app",
	}))
	require.NoError(t, lock.Release())

	err = core.WriteInstallFilesForTest(
		t.Context(),
		stackPath,
		render.RenderedStack{
			ComposeBytes: []byte("new\n"),
			EnvBytes:     []byte("TOKEN=secret\n"),
		},
		nil,
	)
	require.Error(t, err)
	assertUsageValidation(t, err)
	assert.Equal(t, os.FileMode(0o700), fileModePerm(t, stackPath))
}

func TestWriteInstallFilesForTest_RefusesAdditionalFileSymlinkParentEscape(t *testing.T) {
	t.Parallel()

	root := coreTestTempDir(t)
	stackPath := filepath.Join(root, "stacks", "symlink-parent-app")
	outsideDir := filepath.Join(root, "outside")
	require.NoError(t, os.MkdirAll(outsideDir, 0o755))

	outsidePath := filepath.Join(outsideDir, "escape.txt")
	progressCalled := false
	err := core.WriteInstallFilesForTest(
		t.Context(),
		stackPath,
		render.RenderedStack{
			ComposeBytes: []byte("services: {}\n"),
			EnvBytes:     []byte("TOKEN=secret\n"),
			AdditionalFiles: []render.RenderedFile{
				{
					Dest:  "link/escape.txt",
					Mode:  "0644",
					Bytes: []byte("escaped\n"),
				},
			},
		},
		func(step string, _ float64, _ string) {
			if step != types.StepInstallWriteFiles {
				return
			}
			progressCalled = true
			require.NoError(t, os.Symlink(outsideDir, filepath.Join(stackPath, "link")))
		},
	)
	require.Error(t, err)
	assert.True(t, progressCalled)
	assert.NoFileExists(t, outsidePath)
}

func TestWriteInstallFilesForTest_RefusesSymlinkedStackDir(t *testing.T) {
	t.Parallel()

	root := coreTestTempDir(t)
	stackBase := filepath.Join(root, "stacks")
	outsideDir := filepath.Join(root, "outside-stack")
	stackPath := filepath.Join(stackBase, "symlink-stack-app")
	require.NoError(t, os.MkdirAll(stackBase, 0o755))
	require.NoError(t, os.MkdirAll(outsideDir, 0o755))
	require.NoError(t, os.Symlink(outsideDir, stackPath))

	err := core.WriteInstallFilesForTest(
		t.Context(),
		stackPath,
		render.RenderedStack{
			ComposeBytes: []byte("services: {}\n"),
			EnvBytes:     []byte("TOKEN=secret\n"),
		},
		nil,
	)
	require.Error(t, err)
	assertUsageValidation(t, err)
	assert.NoFileExists(t, filepath.Join(outsideDir, "docker-compose.yml"))
	assert.NoFileExists(t, filepath.Join(outsideDir, ".env"))
}

func TestWriteInstallFilesForTest_RefusesReservedAdditionalFileDestBeforeWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		files        []render.RenderedFile
		rejectedPath string
	}{
		{
			name: "env",
			files: []render.RenderedFile{
				{Dest: ".env", Mode: "0644", Bytes: []byte("OVERRIDE=1\n")},
			},
			rejectedPath: ".env",
		},
		{
			name: "compose",
			files: []render.RenderedFile{
				{Dest: "docker-compose.yml", Mode: "0644", Bytes: []byte("services: {}\n")},
			},
			rejectedPath: "docker-compose.yml",
		},
		{
			name: "lock",
			files: []render.RenderedFile{
				{Dest: ".wdm.lock", Mode: "0644", Bytes: []byte("{}\n")},
			},
			rejectedPath: ".wdm.lock",
		},
		{
			name: "backup root child",
			files: []render.RenderedFile{
				{Dest: ".wdm-backups/snapshot", Mode: "0644", Bytes: []byte("backup\n")},
			},
			rejectedPath: filepath.Join(".wdm-backups", "snapshot"),
		},
		{
			name: "backup root",
			files: []render.RenderedFile{
				{Dest: ".wdm-backups/", Mode: "0644", Bytes: []byte("backup\n")},
			},
			rejectedPath: ".wdm-backups",
		},
		{
			name: "compose temp collision",
			files: []render.RenderedFile{
				{Dest: "docker-compose.yml.tmp", Mode: "0644", Bytes: []byte("temp\n")},
			},
			rejectedPath: "docker-compose.yml.tmp",
		},
		{
			name: "env temp collision",
			files: []render.RenderedFile{
				{Dest: ".env.tmp", Mode: "0644", Bytes: []byte("temp\n")},
			},
			rejectedPath: ".env.tmp",
		},
		{
			name: "lock temp collision",
			files: []render.RenderedFile{
				{Dest: ".wdm.lock.tmp", Mode: "0644", Bytes: []byte("temp\n")},
			},
			rejectedPath: ".wdm.lock.tmp",
		},
		{
			name: "env temp child collision",
			files: []render.RenderedFile{
				{Dest: ".env.tmp/value", Mode: "0644", Bytes: []byte("temp child\n")},
			},
			rejectedPath: filepath.Join(".env.tmp", "value"),
		},
		{
			name: "additional temp collision",
			files: []render.RenderedFile{
				{Dest: "config/app.conf", Mode: "0644", Bytes: []byte("ok\n")},
				{Dest: "config/app.conf.tmp", Mode: "0644", Bytes: []byte("temp\n")},
			},
			rejectedPath: filepath.Join("config", "app.conf.tmp"),
		},
		{
			name: "additional temp parent collision",
			files: []render.RenderedFile{
				{Dest: "config/app.conf.tmp/value", Mode: "0644", Bytes: []byte("temp child\n")},
				{Dest: "config/app.conf", Mode: "0644", Bytes: []byte("ok\n")},
			},
			rejectedPath: filepath.Join("config", "app.conf.tmp", "value"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := coreTestTempDir(t)
			stackPath := filepath.Join(root, "stacks", "reserved-"+tt.name)

			err := core.WriteInstallFilesForTest(
				t.Context(),
				stackPath,
				render.RenderedStack{
					ComposeBytes:    []byte("services: {}\n"),
					EnvBytes:        []byte("TOKEN=secret\n"),
					AdditionalFiles: tt.files,
				},
				nil,
			)
			require.Error(t, err)
			assertVerificationFailed(t, err)
			assert.NoFileExists(t, filepath.Join(stackPath, "docker-compose.yml"))
			assert.NoFileExists(t, filepath.Join(stackPath, ".env"))
			assert.NoFileExists(t, filepath.Join(stackPath, ".wdm.lock"))
			assert.NoFileExists(t, filepath.Join(stackPath, tt.rejectedPath))
		})
	}
}

func TestWriteInstallFilesForTest_RefusesDuplicateNormalizedAdditionalFileDestBeforeWrites(t *testing.T) {
	t.Parallel()

	root := coreTestTempDir(t)
	stackPath := filepath.Join(root, "stacks", "duplicate-additional-app")

	err := core.WriteInstallFilesForTest(
		t.Context(),
		stackPath,
		render.RenderedStack{
			ComposeBytes: []byte("services: {}\n"),
			EnvBytes:     []byte("TOKEN=secret\n"),
			AdditionalFiles: []render.RenderedFile{
				{
					Dest:  "config/app.conf",
					Mode:  "0644",
					Bytes: []byte("first\n"),
				},
				{
					Dest:  "./config/app.conf",
					Mode:  "0644",
					Bytes: []byte("second\n"),
				},
			},
		},
		nil,
	)
	require.Error(t, err)
	assertVerificationFailed(t, err)
	assert.NoFileExists(t, filepath.Join(stackPath, "docker-compose.yml"))
	assert.NoFileExists(t, filepath.Join(stackPath, ".env"))
	assert.NoFileExists(t, filepath.Join(stackPath, ".wdm.lock"))
	assert.NoFileExists(t, filepath.Join(stackPath, "config", "app.conf"))
}

// TestInstall_WritesConfigArtifactsBeforeDeployment proves a catalog
// config_generation artifact is rendered from the placeholder map and
// written to disk at its declared dest and mode through the same writer
// arc that commits additional_files (PRD §17, F6).
func TestInstall_WritesConfigArtifactsBeforeDeployment(t *testing.T) {
	t.Parallel()

	app := appFixture("config-gen-app", freeLocalTCPPort(t))
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/config-gen-app/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/config-gen-app/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url"},
		{Name: "SITE_NAME", Type: "string", Required: true},
	}
	app.ConfigGeneration = []catalog.ConfigGenerationArtifact{
		{
			Template: "config/app.toml.tmpl",
			Dest:     "config/app.toml",
			Mode:     "0640",
		},
	}

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/config-gen-app/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
`,
		"templates/config-gen-app/.env.tmpl":            "DB_PASSWORD={{ .DB_PASSWORD }}\nSITE_NAME={{ .SITE_NAME }}\n",
		"templates/config-gen-app/config/app.toml.tmpl": "site = \"{{ .SITE_NAME }}\"\n",
	}, app)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		return "install-secret", nil
	})
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(&fakeDockerClient{}))

	req := types.InstallRequest{
		AppID: app.AppID,
		PlaceholderValues: map[string]string{
			"SITE_NAME": "WDM",
		},
	}
	rendered, err := core.RenderInstallForTest(
		eng,
		t.Context(),
		req,
		system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, rendered.ConfigArtifacts, 1)
	assert.Equal(t, "config/app.toml", rendered.ConfigArtifacts[0].Dest)
	assert.Equal(t, "0640", rendered.ConfigArtifacts[0].Mode)
	assert.Equal(t, []byte("site = \"WDM\"\n"), rendered.ConfigArtifacts[0].Bytes)

	res, err := eng.Install(t.Context(), req, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	stackPath := filepath.Join(filepath.Dir(stateDir), "stacks", app.AppID)
	artifactPath := filepath.Join(stackPath, "config", "app.toml")
	artifactBytes, err := os.ReadFile(artifactPath)
	require.NoError(t, err)
	assert.Equal(t, rendered.ConfigArtifacts[0].Bytes, artifactBytes)
	assert.Equal(t, os.FileMode(0o640), fileModePerm(t, artifactPath))
	assert.NotContains(t, string(artifactBytes), "install-secret")
}

// TestWriteInstallFilesForTest_RefusesConfigArtifactCollidingWithAdditionalFile
// proves the single shared dest tracker refuses a config artifact whose
// dest collides with an additional_files dest, and reports the offending
// kind accurately.
func TestWriteInstallFilesForTest_RefusesConfigArtifactCollidingWithAdditionalFile(t *testing.T) {
	t.Parallel()

	root := coreTestTempDir(t)
	stackPath := filepath.Join(root, "stacks", "config-collide-additional-app")

	err := core.WriteInstallFilesForTest(
		t.Context(),
		stackPath,
		render.RenderedStack{
			ComposeBytes: []byte("services: {}\n"),
			EnvBytes:     []byte("TOKEN=secret\n"),
			AdditionalFiles: []render.RenderedFile{
				{Dest: "config/app.conf", Mode: "0644", Bytes: []byte("additional\n")},
			},
			ConfigArtifacts: []render.RenderedFile{
				{Dest: "config/app.conf", Mode: "0640", Bytes: []byte("config\n")},
			},
		},
		nil,
	)
	require.Error(t, err)
	assertVerificationFailed(t, err)
	assert.Contains(t, err.Error(), "config artifact")
	assert.NoFileExists(t, filepath.Join(stackPath, "config", "app.conf"))
}

// TestWriteInstallFilesForTest_RefusesReservedConfigArtifactDest proves the
// shared tracker refuses a config artifact targeting a reserved file and
// reports it as a config artifact, before any byte is written.
func TestWriteInstallFilesForTest_RefusesReservedConfigArtifactDest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		dest         string
		rejectedPath string
	}{
		{name: "env", dest: ".env", rejectedPath: ".env"},
		{name: "compose", dest: "docker-compose.yml", rejectedPath: "docker-compose.yml"},
		{name: "lock", dest: ".wdm.lock", rejectedPath: ".wdm.lock"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := coreTestTempDir(t)
			stackPath := filepath.Join(root, "stacks", "config-reserved-"+tt.name)

			err := core.WriteInstallFilesForTest(
				t.Context(),
				stackPath,
				render.RenderedStack{
					ComposeBytes: []byte("services: {}\n"),
					EnvBytes:     []byte("TOKEN=secret\n"),
					ConfigArtifacts: []render.RenderedFile{
						{Dest: tt.dest, Mode: "0640", Bytes: []byte("config\n")},
					},
				},
				nil,
			)
			require.Error(t, err)
			assertVerificationFailed(t, err)
			assert.Contains(t, err.Error(), "config artifact")
			assert.NoFileExists(t, filepath.Join(stackPath, "docker-compose.yml"))
			assert.NoFileExists(t, filepath.Join(stackPath, ".env"))
			assert.NoFileExists(t, filepath.Join(stackPath, ".wdm.lock"))
		})
	}
}

// TestWriteInstallFilesForTest_RefusesDuplicateConfigArtifactDest proves two
// config artifacts that normalize to the same dest are refused before any
// write, with a config-artifact diagnostic.
func TestWriteInstallFilesForTest_RefusesDuplicateConfigArtifactDest(t *testing.T) {
	t.Parallel()

	root := coreTestTempDir(t)
	stackPath := filepath.Join(root, "stacks", "duplicate-config-app")

	err := core.WriteInstallFilesForTest(
		t.Context(),
		stackPath,
		render.RenderedStack{
			ComposeBytes: []byte("services: {}\n"),
			EnvBytes:     []byte("TOKEN=secret\n"),
			ConfigArtifacts: []render.RenderedFile{
				{Dest: "config/app.toml", Mode: "0640", Bytes: []byte("first\n")},
				{Dest: "./config/app.toml", Mode: "0640", Bytes: []byte("second\n")},
			},
		},
		nil,
	)
	require.Error(t, err)
	assertVerificationFailed(t, err)
	assert.Contains(t, err.Error(), "config artifact")
	assert.NoFileExists(t, filepath.Join(stackPath, "config", "app.toml"))
}

// configArtifactLeakApp builds an install fixture whose only placeholder is
// a generated secret and whose config_generation artifact echoes it. The
// artifact's mode toggles between a non-0600 sink (refused) and the
// 0600 secret-bearing convention (allowed).
func configArtifactLeakApp(t *testing.T, artifactMode string) (catalog.App, fs.FS) {
	t.Helper()

	app := appFixture("config-leak-app", freeLocalTCPPort(t))
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/config-leak-app/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/config-leak-app/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url"},
	}
	app.ConfigGeneration = []catalog.ConfigGenerationArtifact{
		{
			Template: "config/secret.toml.tmpl",
			Dest:     "config/secret.toml",
			Mode:     artifactMode,
		},
	}

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/config-leak-app/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
`,
		"templates/config-leak-app/.env.tmpl":               "DB_PASSWORD={{ .DB_PASSWORD }}\n",
		"templates/config-leak-app/config/secret.toml.tmpl": "token = \"{{ .DB_PASSWORD }}\"\n",
	}, app)
	return app, catalogFS
}

// TestInstallRender_RejectsGeneratedSecretInNon0600ConfigArtifact pins the
// security crux: a non-0600 config artifact that carries a generated
// secret must fail closed before any sink, and the redactor scrubs the
// secret from the error.
func TestInstallRender_RejectsGeneratedSecretInNon0600ConfigArtifact(t *testing.T) {
	t.Parallel()

	const generated = "config-artifact-generated-secret"

	app, catalogFS := configArtifactLeakApp(t, "0640")
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		return generated, nil
	})

	_, err := core.RenderInstallForTest(
		eng,
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
		nil,
	)
	require.Error(t, err)
	assertVerificationFailed(t, err)
	assert.NotContains(t, err.Error(), generated)
	assertErrorChainDoesNotContain(t, err, generated)
}

// TestInstallRender_Allows0600ConfigArtifactCarryingSecret proves the
// 0600 config artifact is the secret-bearing convention: the same secret
// that is refused in a non-0600 artifact is permitted at 0600 and renders
// to disk verbatim.
func TestInstallRender_Allows0600ConfigArtifactCarryingSecret(t *testing.T) {
	t.Parallel()

	const generated = "config-artifact-0600-secret"

	app, catalogFS := configArtifactLeakApp(t, "0600")
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		return generated, nil
	})
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(&fakeDockerClient{}))

	req := types.InstallRequest{AppID: app.AppID}
	rendered, err := core.RenderInstallForTest(
		eng,
		t.Context(),
		req,
		system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, rendered.ConfigArtifacts, 1)
	assert.Equal(t, "0600", rendered.ConfigArtifacts[0].Mode)
	assert.Contains(t, string(rendered.ConfigArtifacts[0].Bytes), generated)

	res, err := eng.Install(t.Context(), req, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	stackPath := filepath.Join(filepath.Dir(stateDir), "stacks", app.AppID)
	artifactPath := filepath.Join(stackPath, "config", "secret.toml")
	artifactBytes, err := os.ReadFile(artifactPath)
	require.NoError(t, err)
	assert.Contains(t, string(artifactBytes), generated)
	assert.Equal(t, security.SecretFileMode, fileModePerm(t, artifactPath))
}

// TestInstall_NoConfigGenerationLeavesNoConfigArtifacts proves an app
// without config_generation renders and installs unchanged: no config
// artifacts are produced.
func TestInstall_NoConfigGenerationLeavesNoConfigArtifacts(t *testing.T) {
	t.Parallel()

	app := appFixture("no-config-gen-app", freeLocalTCPPort(t))
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/no-config-gen-app/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/no-config-gen-app/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url"},
	}

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/no-config-gen-app/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
`,
		"templates/no-config-gen-app/.env.tmpl": "DB_PASSWORD={{ .DB_PASSWORD }}\n",
	}, app)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		return "install-secret", nil
	})

	rendered, err := core.RenderInstallForTest(
		eng,
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
		nil,
	)
	require.NoError(t, err)
	assert.Empty(t, rendered.ConfigArtifacts)
}

func TestInstall_DeploysWritesManifestAndReturnsResult(t *testing.T) {
	t.Parallel()

	const (
		generatedSecret = "deploy-result-secret"
		containerID     = "0123456789ab"
	)
	digest := "sha256:" + strings.Repeat("a", 64)

	port := freeLocalTCPPort(t)
	app := appFixture("deploy-result-app", port)
	app.ComposeTemplate = "templates/deploy-result-app/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/deploy-result-app/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{
		{Name: "DB_PASSWORD", Type: "secret", Required: true, Encoding: "base64url"},
		{Name: "DOMAIN", Type: "domain", Required: true},
		{Name: "PUBLIC_PORT", Type: "port", Required: false, Default: port},
	}
	app.Networks = []catalog.Network{{Name: "wdm_front", Internal: false}}
	app.LocalTargetURLTemplate = "http://127.0.0.1:{{ .PUBLIC_PORT }}/"
	app.FirstRunNotes = []string{"Open the local URL and create the admin account."}
	app.CompletedServices = []string{"app"}
	app.Resources = []catalog.ResourceProfile{{
		Service:       "app",
		Memory:        catalog.MemoryBand{Min: "128m", Recommended: "256m", Max: "1g"},
		CPUs:          catalog.CPUBand{Min: "0.1", Recommended: "0.5", Max: "2.0"},
		PIDs:          catalog.PIDsBand{Default: 100, Max: 200},
		AllowOverride: true,
	}}

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/deploy-result-app/docker-compose.yml.tmpl": `services:
  app:
    image: docker.io/example/app:1.0.0
    volumes:
      - ./data:/app/data
`,
		"templates/deploy-result-app/.env.tmpl": "DB_PASSWORD={{ .DB_PASSWORD }}\nDOMAIN={{ .DOMAIN }}\n",
	}, app)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})
	core.SetInstallSecretGeneratorForTest(eng, func(security.Encoding) (string, error) {
		return generatedSecret, nil
	})

	stackPath := filepath.Join(filepath.Dir(stateDir), "stacks", app.AppID)
	stackLockPath := filepath.Join(stackPath, ".wdm.lock")

	fake := &fakeDockerClient{}
	fake.runFn = func(call int, _ docker.Invocation) (docker.CommandResult, error) {
		switch call {
		case 2:
			return missingNetworkResult("wdm_front")
		case 5:
			return docker.CommandResult{Stdout: "docker.io/example/app@" + digest + "\n"}, nil
		case 6:
			return docker.CommandResult{Stdout: containerID + "\n"}, nil
		case 7:
			return docker.CommandResult{
				Stdout: managedContainerInspectStdout(t, "wdm-app-1", "app", app.AppID, port),
			}, nil
		default:
			return docker.CommandResult{}, nil
		}
	}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			// The per-stack flock must be held across the full
			// write → confirm → deploy → manifest span.
			f, openErr := os.OpenFile(stackLockPath, os.O_RDWR, 0)
			require.NoError(t, openErr)
			defer f.Close() // best-effort flock probe cleanup
			acquired, lockErr := state.TryLockExclusive(f)
			require.NoError(t, lockErr)
			assert.False(t, acquired, "per-stack flock must be held at confirmation time")
			return true, nil
		},
	}

	before := time.Now().UTC()
	var steps []string
	res, err := eng.Install(
		t.Context(),
		types.InstallRequest{AppID: app.AppID, Domain: "status.example.com"},
		func(step string, _ float64, _ string) { steps = append(steps, step) },
		confirmer,
	)
	require.NoError(t, err)
	require.NotNil(t, res)

	// Step IDs cover deploy, lock update, and status in order.
	deployIdx := stepIndex(t, steps, types.StepInstallDeploy)
	lockIdx := stepIndex(t, steps, types.StepInstallLockUpdate)
	statusIdx := stepIndex(t, steps, types.StepInstallStatus)
	assert.Less(t, stepIndex(t, steps, types.StepInstallNetworkCreate), deployIdx)
	assert.Less(t, deployIdx, lockIdx)
	assert.Less(t, lockIdx, statusIdx)

	// Install issues up -d, never pull, and never any down shape.
	assert.Contains(t, fake.invocationTypes, "docker.composeUpInvocation")
	assert.NotContains(t, fake.invocationTypes, "docker.composePullInvocation")
	assert.NotContains(t, fake.invocationTypes, "docker.composeDownInvocation")
	assert.Equal(t, 7, fake.calls)

	// Confirmation surfaced the volumes the deployment creates.
	require.Len(t, confirmer.calls, 1)
	assert.Contains(t, confirmer.calls[0].Message, "creates volume ./data:/app/data")
	assert.NotContains(t, confirmer.calls[0].Message, generatedSecret)

	// Result carries Compose project, ports, status, and guidance.
	assert.Equal(t, app.AppID, res.AppID)
	assert.Equal(t, stackPath, res.StackPath)
	assert.Equal(t, "wdm-"+app.AppID, res.ComposeProject)
	assert.Equal(t, []string{"app"}, res.StartedServices)
	require.Len(t, res.LocalPorts, 1)
	assert.Equal(t, port, res.LocalPorts[0].HostPort)
	require.NotNil(t, res.PostInstallGuidance)
	assert.Equal(t, fmt.Sprintf("http://127.0.0.1:%d/", port), res.PostInstallGuidance.LocalTargetURL)
	assert.Equal(t, []string{"Open the local URL and create the admin account."}, res.PostInstallGuidance.FirstRunNotes)
	require.NotNil(t, res.PostInstallGuidance.Pangolin)
	assert.Equal(t, "http://127.0.0.1:8080", res.PostInstallGuidance.Pangolin.TargetURL)
	assert.Equal(t, "app", res.PostInstallGuidance.Pangolin.RecommendedSubdomain)
	assert.Equal(t, []string{"Guide: https://example.invalid"}, res.PostInstallGuidance.Pangolin.Notes)
	require.NotNil(t, res.Status)
	assert.Equal(t, "running", res.Status.State)
	assert.False(t, res.Status.NeedsAttention)
	assert.Empty(t, res.Status.AttentionReasons)
	require.Len(t, res.Status.Services, 1)
	assert.Equal(t, "app", res.Status.Services[0].Service)
	assert.Equal(t, "wdm-app-1", res.Status.Services[0].ContainerName)
	assert.Equal(t, "running", res.Status.Services[0].State)
	require.NotNil(t, res.Status.UpdatedAt)

	// The.wdm.lock manifest is durable with the full PRD §9/§30 set.
	lock, err := state.ReadStackLock(t.Context(), stackLockPath)
	require.NoError(t, err)
	assert.Equal(t, 1, lock.SchemaVersion)
	assert.Equal(t, app.AppID, lock.AppID)
	assert.Equal(t, app.TemplateName, lock.TemplateName)
	assert.Equal(t, "2026.05.29", lock.TemplateVersion)
	assert.Equal(t, "stable", lock.CatalogChannel)
	assert.Equal(t, "2026-05-29T00:00:00Z", lock.CatalogVersion)
	assert.Equal(t, stackPath, lock.StackPath)
	assert.Equal(t, "status.example.com", lock.SelectedDomain)
	assert.Equal(t, []int{port}, lock.LocalPorts)
	assert.Equal(t, "wdm-"+app.AppID, lock.ComposeProject)
	require.Len(t, lock.ImagePins, 1)
	assert.Equal(t, "app", lock.ImagePins[0].Service)
	assert.Equal(t, "docker.io/example/app", lock.ImagePins[0].Image)
	assert.Equal(t, "1.0.0", lock.ImagePins[0].Tag)
	assert.Equal(t, digest, lock.ImagePins[0].Digest)
	assert.Equal(t, []string{"DB_PASSWORD"}, lock.GeneratedFields)
	assert.Equal(t, []string{"app"}, lock.CompletedServices)
	require.NotNil(t, lock.LastSuccessfulOperation)
	assert.Equal(t, "install", lock.LastSuccessfulOperation.Kind)
	assert.Equal(t, "dev", lock.LastSuccessfulOperation.WDMVersion)
	assert.WithinDuration(t, before, lock.LastSuccessfulOperation.At, time.Minute)
	require.NotNil(t, lock.RecommendedResources)
	assert.Equal(t, uint64(256*1024*1024), lock.RecommendedResources.MemoryBytes)
	assert.InDelta(t, 0.5, lock.RecommendedResources.CPUs, 0.0001)

	// The flock is released after the happy path completes.
	f, err := os.OpenFile(stackLockPath, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close() // best-effort flock probe cleanup
	acquired, err := state.TryLockExclusive(f)
	require.NoError(t, err)
	assert.True(t, acquired, "per-stack flock must be released after install")

	// No generated secret reaches the result surface.
	rawResult, err := json.Marshal(res)
	require.NoError(t, err)
	assert.NotContains(t, string(rawResult), generatedSecret)
}

func TestInstall_DeployFailureRollsBackOnlyThisInstallsDockerResources(t *testing.T) {
	t.Parallel()

	app := appFixture("deploy-fail-app", freeLocalTCPPort(t))
	app.Networks = []catalog.Network{{Name: "wdm_front", Internal: false}}
	eng, stackPath := newInstallDeployTestEngine(t, app)

	const (
		volumeData  = "wdm-deploy-fail-app_data"
		volumeCache = "wdm-deploy-fail-app_cache"
	)
	upErr := errors.New("compose up exploded")
	fake := &fakeDockerClient{
		runFn: func(call int, _ docker.Invocation) (docker.CommandResult, error) {
			switch call {
			case 2:
				// The catalog network is missing, so EnsureNetworkReport
				// takes its create path and records it for rollback.
				return missingNetworkResult("wdm_front")
			case 4:
				return docker.CommandResult{}, upErr
			case 6:
				// Project-labeled named-volume listing during rollback.
				return docker.CommandResult{Stdout: volumeData + "\n" + volumeCache + "\n"}, nil
			default:
				return docker.CommandResult{}, nil
			}
		},
	}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, upErr)

	// Pre-manifest rollback: safe compose down (never -v), then the
	// project-labeled named volumes, then the network this install
	// created — in that order — before the partial files are removed.
	require.Equal(t, []string{
		"docker.composeConfigInvocation",     // 1: pre-exposure validation
		"docker.networkInspectInvocation",    // 2: ensure network (missing)
		"docker.networkCreateInvocation",     // 3: created → tracked
		"docker.composeUpInvocation",         // 4: deploy fault
		"docker.composeDownInvocation",       // 5: rollback step 1
		"docker.projectVolumeListInvocation", // 6: rollback step 2
		"docker.removeNamedVolumeInvocation", // 7: volume cache (sorted)
		"docker.removeNamedVolumeInvocation", // 8: volume data
		"docker.removeNetworkInvocation",     // 9: created network
	}, fake.invocationTypes)
	assert.Equal(t, 9, fake.calls)

	// All partial files and the created stack directory are removed.
	assert.NoFileExists(t, filepath.Join(stackPath, "docker-compose.yml"))
	assert.NoFileExists(t, filepath.Join(stackPath, ".env"))
	assert.NoFileExists(t, filepath.Join(stackPath, ".wdm.lock"))
	assert.NoDirExists(t, stackPath)
}

func TestInstall_DeployFailureRemovesCreatedNetworksInReverseOrder(t *testing.T) {
	t.Parallel()

	app := appFixture("reverse-net-app", freeLocalTCPPort(t))
	const (
		networkFront = "wdm_front"
		networkBack  = "wdm_back"
	)
	app.Networks = []catalog.Network{
		{Name: networkFront, Internal: false},
		{Name: networkBack, Internal: true},
	}
	eng, stackPath := newInstallDeployTestEngine(t, app)

	upErr := errors.New("compose up exploded")
	fake := &fakeDockerClient{
		runFn: func(call int, _ docker.Invocation) (docker.CommandResult, error) {
			switch call {
			case 2:
				// First network missing → created and tracked first.
				return missingNetworkResult(networkFront)
			case 4:
				// Second network missing → created and tracked second.
				return missingNetworkResult(networkBack)
			case 6:
				return docker.CommandResult{}, upErr
			default:
				return docker.CommandResult{}, nil
			}
		},
	}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, upErr)

	// Both networks were created here, so both are removed — but in REVERSE
	// creation order: the last-created (wdm_back) is torn down before the
	// first-created (wdm_front), so a later network that depends on an
	// earlier one is never the lingering leftover.
	require.Equal(t, []string{
		"docker.composeConfigInvocation",     // 1: pre-exposure validation
		"docker.networkInspectInvocation",    // 2: ensure wdm_front (missing)
		"docker.networkCreateInvocation",     // 3: wdm_front created → tracked
		"docker.networkInspectInvocation",    // 4: ensure wdm_back (missing)
		"docker.networkCreateInvocation",     // 5: wdm_back created → tracked
		"docker.composeUpInvocation",         // 6: deploy fault
		"docker.composeDownInvocation",       // 7: rollback step 1
		"docker.projectVolumeListInvocation", // 8: rollback step 2 (no volumes)
		"docker.removeNetworkInvocation",     // 9: wdm_back removed first
		"docker.removeNetworkInvocation",     // 10: wdm_front removed second
	}, fake.invocationTypes)

	// The two removals target the networks in reverse creation order.
	firstRemoveIdx := 8
	secondRemoveIdx := 9
	assert.Contains(t, fake.invocationDetails[firstRemoveIdx], networkBack)
	assert.NotContains(t, fake.invocationDetails[firstRemoveIdx], networkFront)
	assert.Contains(t, fake.invocationDetails[secondRemoveIdx], networkFront)

	assert.NoDirExists(t, stackPath)
}

func TestInstall_DeployFailureDoesNotRemovePreExistingNetwork(t *testing.T) {
	t.Parallel()

	app := appFixture("preexisting-net-app", freeLocalTCPPort(t))
	app.Networks = []catalog.Network{{Name: "wdm_front", Internal: false}}
	eng, stackPath := newInstallDeployTestEngine(t, app)

	upErr := errors.New("compose up exploded")
	fake := &fakeDockerClient{
		runFn: func(call int, _ docker.Invocation) (docker.CommandResult, error) {
			switch call {
			case 2:
				// The network already exists with a matching internal
				// flag: EnsureNetworkReport reconciles it (created=false)
				// and the rollback must leave it alone.
				return docker.CommandResult{Stdout: "false\n"}, nil
			case 3:
				return docker.CommandResult{}, upErr
			default:
				return docker.CommandResult{}, nil
			}
		},
	}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, upErr)

	// The deploy was attempted, so compose down and volume listing run —
	// but the pre-existing network was never created here, so it is
	// never removed.
	assert.Contains(t, fake.invocationTypes, "docker.composeDownInvocation")
	assert.Contains(t, fake.invocationTypes, "docker.projectVolumeListInvocation")
	assert.NotContains(t, fake.invocationTypes, "docker.removeNetworkInvocation")
	assert.NoDirExists(t, stackPath)
}

func TestInstall_DeployFailureCleanupFaultJoinsOriginalError(t *testing.T) {
	t.Parallel()

	app := appFixture("cleanup-join-app", freeLocalTCPPort(t))
	eng, stackPath := newInstallDeployTestEngine(t, app)

	upErr := errors.New("compose up exploded")
	downErr := errors.New("compose down refused")
	fake := &fakeDockerClient{
		runFn: func(call int, _ docker.Invocation) (docker.CommandResult, error) {
			switch call {
			case 2:
				return docker.CommandResult{}, upErr
			case 3:
				// The rollback's safe compose down itself fails.
				return docker.CommandResult{}, downErr
			default:
				return docker.CommandResult{}, nil
			}
		},
	}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)

	// The original deploy fault stays discoverable, and the rollback
	// fault is joined alongside it naming the compose project and stack
	// path so the inconsistency is visible (PRD §18).
	require.ErrorIs(t, err, upErr)
	require.ErrorIs(t, err, downErr)
	assert.Contains(t, err.Error(), "wdm-"+app.AppID)
	assert.Contains(t, err.Error(), stackPath)
	assert.Contains(t, fake.invocationTypes, "docker.composeDownInvocation")
}

func TestInstall_DigestAbsenceDoesNotFailInstall(t *testing.T) {
	t.Parallel()

	app := appFixture("digest-absent-app", freeLocalTCPPort(t))
	eng, stackPath := newInstallDeployTestEngine(t, app)

	fake := &fakeDockerClient{
		runFn: func(call int, _ docker.Invocation) (docker.CommandResult, error) {
			if call == 3 {
				// Image digest inspect fails the way a registry-less
				// host would; capture stays opportunistic.
				return docker.CommandResult{
					Stderr:   "Error: No such image: docker.io/example/app:1.0.0",
					ExitCode: 1,
				}, errors.New("exit status 1")
			}
			return docker.CommandResult{}, nil
		},
	}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	lock, err := state.ReadStackLock(t.Context(), filepath.Join(stackPath, ".wdm.lock"))
	require.NoError(t, err)
	require.Len(t, lock.ImagePins, 1)
	assert.Equal(t, "1.0.0", lock.ImagePins[0].Tag)
	assert.Empty(t, lock.ImagePins[0].Digest)
}

func TestInstall_StatusVerificationFailureMarksNeedsAttentionWithoutFailing(t *testing.T) {
	t.Parallel()

	app := appFixture("status-fail-app", freeLocalTCPPort(t))
	eng, stackPath := newInstallDeployTestEngine(t, app)

	fake := &fakeDockerClient{
		runFn: func(call int, _ docker.Invocation) (docker.CommandResult, error) {
			if call == 4 {
				return docker.CommandResult{ExitCode: 1}, errors.New("docker daemon hiccup")
			}
			return docker.CommandResult{}, nil
		},
	}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	// Post-commit verification trouble is an explicit needs-attention
	// state, never a silent partial success and never a file removal
	// (the step 6 manifest fsync already made the install durable).
	require.NotNil(t, res.Status)
	assert.True(t, res.Status.NeedsAttention)
	assert.Equal(t, []string{"status_check_failed"}, res.Status.AttentionReasons)
	assert.Empty(t, res.StartedServices)

	// The manifest is durable, so the post-commit failure path never
	// rolls back Docker state: no compose down, no volume listing, no
	// resource removal.
	assert.NotContains(t, fake.invocationTypes, "docker.composeDownInvocation")
	assert.NotContains(t, fake.invocationTypes, "docker.projectVolumeListInvocation")
	assert.NotContains(t, fake.invocationTypes, "docker.removeNamedVolumeInvocation")
	assert.NotContains(t, fake.invocationTypes, "docker.removeNetworkInvocation")

	require.FileExists(t, filepath.Join(stackPath, "docker-compose.yml"))
	lock, err := state.ReadStackLock(t.Context(), filepath.Join(stackPath, ".wdm.lock"))
	require.NoError(t, err)
	require.NotNil(t, lock.LastSuccessfulOperation)
	assert.Equal(t, "install", lock.LastSuccessfulOperation.Kind)
}

func TestInstall_NeedsAttentionWhenManagedContainerMissing(t *testing.T) {
	t.Parallel()

	app := appFixture("missing-container-app", freeLocalTCPPort(t))
	eng, _ := newInstallDeployTestEngine(t, app)

	// Zero-value fake: container listing returns no containers, so the
	// expected service is missing and the planned port is unpublished.
	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	require.NotNil(t, res.Status)
	assert.True(t, res.Status.NeedsAttention)
	assert.Equal(t, "needs_attention", res.Status.State)
	assert.Contains(t, res.Status.AttentionReasons, "container_missing")
	assert.Contains(t, res.Status.AttentionReasons, "port_mismatch")
	assert.Empty(t, res.StartedServices)
	require.Len(t, res.Status.Services, 1)
	assert.True(t, res.Status.Services[0].NeedsAttention)
}

// TestInstallRender_VerifiesCompletedServicesMatchCatalog drives the
// install-time cross-check: every completed_services entry must match
// the conservative compose-service shape, sit in image_pins, and render
// as a real compose service. The accept case mirrors stoat's two init
// containers (mongo-init, garage-init); each reject case violates one
// rule and must fail the install closed with ErrCodeVerificationFailed.
func TestInstallRender_VerifiesCompletedServicesMatchCatalog(t *testing.T) {
	t.Parallel()

	const composeTemplate = `services:
  app:
    image: docker.io/example/app:1.0.0
  mongo-init:
    image: docker.io/example/mongo-init:1.0.0
  garage-init:
    image: docker.io/example/garage-init:1.0.0
`

	newCompletedApp := func(completed []string) catalog.App {
		app := appFixture("completed-services-app", freeLocalTCPPort(t))
		app.Ports = []catalog.Port{}
		app.ComposeTemplate = "templates/completed/docker-compose.yml.tmpl"
		app.EnvTemplate = "templates/completed/.env.tmpl"
		app.Placeholders = []catalog.Placeholder{}
		app.ImagePins = []catalog.ImagePin{
			{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
			{Service: "mongo-init", Image: "docker.io/example/mongo-init", Tag: "1.0.0"},
			{Service: "garage-init", Image: "docker.io/example/garage-init", Tag: "1.0.0"},
		}
		app.CompletedServices = completed
		return app
	}

	renderApp := func(t *testing.T, app catalog.App) error {
		t.Helper()

		catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
			"templates/completed/docker-compose.yml.tmpl": composeTemplate,
			"templates/completed/.env.tmpl":               "",
		}, app)
		eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
		_, err := core.RenderInstallForTest(
			eng,
			t.Context(),
			types.InstallRequest{AppID: app.AppID},
			system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte},
			nil,
		)
		return err
	}

	t.Run("accepts stoat-style init services", func(t *testing.T) {
		t.Parallel()

		err := renderApp(t, newCompletedApp([]string{"mongo-init", "garage-init"}))
		require.NoError(t, err)
	})

	t.Run("rejects service absent from image_pins", func(t *testing.T) {
		t.Parallel()

		app := newCompletedApp([]string{"mongo-init"})
		// Drop the mongo-init pin so the completed service has no pin, even
		// though it still renders in the compose template.
		app.ImagePins = []catalog.ImagePin{
			{Service: "app", Image: "docker.io/example/app", Tag: "1.0.0"},
			{Service: "garage-init", Image: "docker.io/example/garage-init", Tag: "1.0.0"},
		}
		err := renderApp(t, app)
		require.Error(t, err)
		assertVerificationFailed(t, err)
		assert.Contains(t, err.Error(), "absent from image_pins")
	})

	t.Run("rejects service absent from rendered compose", func(t *testing.T) {
		t.Parallel()

		app := newCompletedApp([]string{"ghost-init"})
		// Pin the phantom service so it clears the image_pins check and is
		// rejected only at the rendered-compose cross-reference.
		app.ImagePins = append(app.ImagePins, catalog.ImagePin{
			Service: "ghost-init",
			Image:   "docker.io/example/ghost-init",
			Tag:     "1.0.0",
		})
		err := renderApp(t, app)
		require.Error(t, err)
		assertVerificationFailed(t, err)
		assert.Contains(t, err.Error(), "absent from the rendered compose")
	})

	t.Run("rejects invalid compose service name", func(t *testing.T) {
		t.Parallel()

		err := renderApp(t, newCompletedApp([]string{"../escape"}))
		require.Error(t, err)
		assertVerificationFailed(t, err)
		assert.Contains(t, err.Error(), "invalid compose service")
	})
}

// TestInstall_CompletedInitServiceReportsRunningNotAttention is the
// install-time parity proof: a fresh install whose only non-running
// service is a completed init container that exited 0 reports the app
// "running", not "needs_attention".
func TestInstall_CompletedInitServiceReportsRunningNotAttention(t *testing.T) {
	t.Parallel()

	port := freeLocalTCPPort(t)
	app := appFixture("completed-init-app", port)
	app.Ports = []catalog.Port{}
	app.ComposeTemplate = "templates/completed-init/docker-compose.yml.tmpl"
	app.EnvTemplate = "templates/completed-init/.env.tmpl"
	app.Placeholders = []catalog.Placeholder{}
	app.ImagePins = []catalog.ImagePin{
		{Service: "init", Image: "docker.io/example/init", Tag: "1.0.0"},
	}
	app.CompletedServices = []string{"init"}

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		"templates/completed-init/docker-compose.yml.tmpl": "services:\n  init:\n    image: docker.io/example/init:1.0.0\n",
		"templates/completed-init/.env.tmpl":               "",
	}, app)
	eng, _ := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})

	digest := "sha256:" + strings.Repeat("b", 64)
	fake := &fakeDockerClient{
		runFn: func(_ int, inv docker.Invocation) (docker.CommandResult, error) {
			switch fmt.Sprintf("%T", inv) {
			case "docker.imageDigestInspectInvocation":
				return docker.CommandResult{Stdout: "docker.io/example/init@" + digest + "\n"}, nil
			case "docker.projectContainerListInvocation":
				return docker.CommandResult{Stdout: "0123456789ab\n"}, nil
			case "docker.containerInspectInvocation":
				return docker.CommandResult{
					Stdout: completedContainerInspectStdout(t, "wdm-init-1", "init", app.AppID),
				}, nil
			default:
				return docker.CommandResult{}, nil
			}
		},
	}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.NoError(t, err)
	require.NotNil(t, res)

	require.NotNil(t, res.Status)
	assert.Equal(t, "running", res.Status.State)
	assert.False(t, res.Status.NeedsAttention)
	assert.Empty(t, res.Status.AttentionReasons)
	require.Len(t, res.Status.Services, 1)
	assert.Equal(t, "init", res.Status.Services[0].Service)
	assert.Equal(t, "completed", res.Status.Services[0].State)
	assert.False(t, res.Status.Services[0].NeedsAttention)
}

// completedContainerInspectStdout fabricates the 8-line inspect output
// for a wdm-managed container that exited cleanly (status "exited",
// not running, exit code 0) with no published ports — the on-disk shape
// of a one-shot init container that completed by design.
func completedContainerInspectStdout(t *testing.T, name, service, appID string) string {
	t.Helper()

	labels, err := json.Marshal(map[string]string{
		"com.docker.compose.service": service,
		"wdm.managed":                "true",
		"wdm.app":                    appID,
	})
	require.NoError(t, err)
	return fmt.Sprintf("%q\n%s\n\"exited\"\nfalse\nfalse\n0\n\"\"\n{}\n", "/"+name, labels)
}

func TestInstall_ContextCancellationMidDeployRunsRollbackUnderDetachedContext(t *testing.T) {
	t.Parallel()

	app := appFixture("cancel-deploy-app", freeLocalTCPPort(t))
	eng, stackPath := newInstallDeployTestEngine(t, app)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// honorCtx makes the fake fail closed on a canceled context exactly as
	// the production docker client does. The deploy (call 2) cancels the
	// install context and reports the cancellation, so WITHOUT the
	// cancellation-detached rollback context the rollback calls would
	// inherit the canceled ctx and short-circuit, orphaning whatever `up`
	// created. This test proves the rollback runs under a live, detached
	// context instead.
	fake := &fakeDockerClient{
		honorCtx: true,
		runFn: func(call int, _ docker.Invocation) (docker.CommandResult, error) {
			if call == 2 {
				cancel()
				return docker.CommandResult{}, context.Canceled
			}
			return docker.CommandResult{}, nil
		},
	}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	res, err := eng.Install(ctx, types.InstallRequest{AppID: app.AppID}, nil, &fakeConfirmer{})
	require.Error(t, err)
	assert.Nil(t, res)
	require.ErrorIs(t, err, context.Canceled)

	// The deploy was attempted before cancellation, so the pre-manifest
	// rollback runs (safe compose down + project-volume listing) to reclaim
	// any containers or volumes `up` may have partially created.
	downIdx := stepIndex(t, fake.invocationTypes, "docker.composeDownInvocation")
	listIdx := stepIndex(t, fake.invocationTypes, "docker.projectVolumeListInvocation")

	// The proof: the rollback calls ran under a context that survives the
	// canceled install. With the detached context they observe no
	// cancellation; without it (the pre-fix behavior) the fail-closed fake
	// would have seen the canceled install ctx and refused the cleanup.
	require.NoError(t, fake.ctxErrs[downIdx],
		"compose down must run under a live, cancellation-detached context")
	require.NoError(t, fake.ctxErrs[listIdx],
		"project-volume listing must run under a live, cancellation-detached context")

	// The manifest was never durable, so the canceled deploy leaves no
	// stack directory, compose file, env file, or .wdm.lock behind.
	assert.NoDirExists(t, stackPath)
}

func TestInstall_CleanupFailureSurfacesLeftoverPathAlongOriginalFault(t *testing.T) {
	t.Parallel()

	app := appFixture("cleanup-fail-app", freeLocalTCPPort(t))
	eng, stackPath := newInstallDeployTestEngine(t, app)

	fake := &fakeDockerClient{}
	core.SetInstallDockerClientFactoryForTest(eng, fakeDockerClientFactory(fake))

	// A foreign file appears in the stack directory before the
	// decline: cleanup removes ONLY wdm-written artifacts (os.Remove
	// refuses non-empty directories), so the directory removal fails
	// and the inconsistency is surfaced alongside the original fault
	// (PRD §18 fail-closed on uncertain state).
	foreignPath := filepath.Join(stackPath, "user-owned.txt")
	confirmer := &fakeConfirmer{
		confirmFn: func(context.Context, types.Confirmation) (bool, error) {
			require.NoError(t, os.WriteFile(foreignPath, []byte("keep me\n"), 0o644))
			return false, nil
		},
	}

	res, err := eng.Install(t.Context(), types.InstallRequest{AppID: app.AppID}, nil, confirmer)
	require.Error(t, err)
	assert.Nil(t, res)

	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeUserCanceled, typedErr.Code)
	assert.Contains(t, err.Error(), "install cleanup could not remove all partial files")

	// wdm-written artifacts are gone; the user's file is untouched.
	assert.NoFileExists(t, filepath.Join(stackPath, "docker-compose.yml"))
	assert.NoFileExists(t, filepath.Join(stackPath, ".env"))
	assert.NoFileExists(t, filepath.Join(stackPath, ".wdm.lock"))
	require.FileExists(t, foreignPath)
	raw, readErr := os.ReadFile(foreignPath)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("keep me\n"), raw)
}

func TestInstallPlan_ResourceGuidanceIgnoresSiblingStackLocks(t *testing.T) {
	t.Parallel()

	port := freeLocalTCPPort(t)
	app := appFixture("budget-app", port)
	app.Resources = []catalog.ResourceProfile{{
		Service:       "app",
		Memory:        catalog.MemoryBand{Min: "128m", Recommended: "2g", Max: "4g"},
		CPUs:          catalog.CPUBand{Min: "0.1", Recommended: "1.0", Max: "2.0"},
		PIDs:          catalog.PIDsBand{Default: 100, Max: 200},
		AllowOverride: true,
	}}
	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		app.ComposeTemplate: "services:\n  app:\n    image: docker.io/example/app:1.0.0\n",
		app.EnvTemplate:     "",
	}, app)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))

	// Sibling manifests are stack state, not host-capacity reservations.
	// Even extreme recorded guidance must not prevent another app from using
	// its own recommended limits when the host itself can fit them.
	existingStack := filepath.Join(filepath.Dir(stateDir), "stacks", "existing-app")
	require.NoError(t, os.MkdirAll(existingStack, 0o755))
	existingLock, err := state.AcquireStackLock(t.Context(), filepath.Join(existingStack, ".wdm.lock"))
	require.NoError(t, err)
	require.NoError(t, existingLock.Write(state.StackLock{
		SchemaVersion:  1,
		AppID:          "existing-app",
		TemplateName:   "existing-app",
		CatalogChannel: "stable",
		StackPath:      existingStack,
		ComposeProject: "wdm-existing-app",
		RecommendedResources: &state.RecommendedResources{
			MemoryBytes: 6 * gibibyte,
			CPUs:        3.0,
		},
	}))
	require.NoError(t, existingLock.Release())

	stackBase := filepath.Join(filepath.Dir(stateDir), "stacks")
	writeCoreStackFixture(t, stackBase, "broken-app", fmt.Sprintf(
		`{"schema_version": 1, "recommended_resources": {"memory_bytes": %d,`, 6*gibibyte))

	host := system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}
	var steps []string
	snapshot, err := core.PlanInstallForTest(
		eng,
		t.Context(),
		types.InstallRequest{AppID: app.AppID},
		host,
		func(step string, _ float64, _ string) { steps = append(steps, step) },
	)
	require.NoError(t, err)
	assert.Equal(t, "2g", snapshot.ResolvedValues["MEMORY_LIMIT_APP"])
	assert.NotContains(t, steps, types.StepInstallResourceDegraded)
}

// managedContainerInspectStdout fabricates the exact 8-line safe-field
// output internal/docker's container inspection parses, labeled as a
// wdm-managed container for the given service and app.
func managedContainerInspectStdout(t *testing.T, name, service, appID string, hostPort int) string {
	t.Helper()

	labels, err := json.Marshal(map[string]string{
		"com.docker.compose.service": service,
		"wdm.managed":                "true",
		"wdm.app":                    appID,
	})
	require.NoError(t, err)
	ports := fmt.Sprintf(`{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"%d"}]}`, hostPort)
	return fmt.Sprintf("%q\n%s\n\"running\"\ntrue\nfalse\n0\n\"\"\n%s\n", "/"+name, labels, ports)
}

func catalogFixtureFS(t *testing.T, apps ...catalog.App) fs.FS {
	t.Helper()

	return catalogFixtureFSWithFiles(t, nil, apps...)
}

func catalogFixtureFSWithFiles(t *testing.T, files map[string]string, apps ...catalog.App) fs.FS {
	t.Helper()

	manifest := catalog.Catalog{
		SchemaVersion: 1,
		Channel:       "stable",
		GeneratedAt:   time.Date(2026, time.May, 29, 0, 0, 0, 0, time.UTC),
		Apps:          apps,
	}
	raw, err := yaml.Marshal(manifest)
	require.NoError(t, err)

	catalogFS := fstest.MapFS{
		"stable/catalog.yaml": &fstest.MapFile{Data: raw},
	}
	for name, contents := range files {
		catalogFS[name] = &fstest.MapFile{Data: []byte(contents)}
	}
	return catalogFS
}

func appFixture(appID string, hostPort int) catalog.App {
	return catalog.App{
		AppID:           appID,
		Name:            "Test app",
		Summary:         "Test summary",
		Description:     "Test description",
		TemplateName:    appID,
		TemplateVersion: "2026.05.29",
		ComposeTemplate: "templates/test/docker-compose.yml.tmpl",
		EnvTemplate:     "templates/test/.env.tmpl",
		Placeholders:    []catalog.Placeholder{},
		SupportedVersions: catalog.SupportedVersions{
			Docker:  ">=20.10",
			Compose: ">=2.0",
		},
		Ports: []catalog.Port{
			{
				Service:   "app",
				Container: 8080,
				Host:      hostPort,
				Protocol:  "tcp",
			},
		},
		ImagePins: []catalog.ImagePin{
			{
				Service: "app",
				Image:   "docker.io/example/app",
				Tag:     "1.0.0",
			},
		},
		PangolinGuidance: catalog.PangolinGuidance{
			TargetURL:            "http://127.0.0.1:8080",
			RecommendedSubdomain: "app",
			Notes:                []string{"Guide: https://example.invalid"},
		},
		RiskClassification: []string{"safe"},
	}
}

// newInstallDeployTestEngine builds an engine over a minimal catalog
// fixture for app (no secret placeholders) with the host resource
// probe stubbed, returning the engine plus the default planned stack
// path for the app.
func newInstallDeployTestEngine(t *testing.T, app catalog.App) (*core.Engine, string) {
	t.Helper()

	catalogFS := catalogFixtureFSWithFiles(t, map[string]string{
		app.ComposeTemplate: "services:\n  app:\n    image: docker.io/example/app:1.0.0\n",
		app.EnvTemplate:     "",
	}, app)
	eng, stateDir := newTestEngine(t, core.WithCatalog(catalogFS))
	core.SetInstallHostResourceProbeForTest(eng, func() (system.HostResources, error) {
		return system.HostResources{CPUCores: 4, TotalMemoryBytes: 8 * gibibyte}, nil
	})
	return eng, filepath.Join(filepath.Dir(stateDir), "stacks", app.AppID)
}

// fakeDockerClient scripts docker invocation outcomes by call number
// so install tests can drive compose validation, network pre-creation,
// deployment, and inspection without a Docker daemon. The zero value
// succeeds on every call. invocationTypes records the concrete
// invocation type of every call (e.g. "docker.composeUpInvocation")
// so tests can assert deployment shape — including that install never
// issues a compose down or pull invocation.
type fakeDockerClient struct {
	runFn           func(call int, inv docker.Invocation) (docker.CommandResult, error)
	streamFn        func(ctx context.Context, inv docker.Invocation, sink docker.RawLogSink) error
	calls           int
	streamCalls     int
	invocationTypes []string
	// invocationDetails records the %+v rendering of every invocation
	// (including unexported fields such as a network name) so a test can
	// assert not just the invocation type but the concrete target — used to
	// pin the reverse-order network removal.
	invocationDetails []string
	// honorCtx makes Run fail closed on a canceled context exactly as the
	// real execClient does (internal/docker/client.go) — a canceled ctx
	// short-circuits the call before any work. It is opt-in so the default
	// fake stays ctx-agnostic for the existing tests; the cancellation
	// rollback test sets it to PROVE the rollback runs under a context
	// that survives parent cancellation.
	honorCtx bool
	// ctxErrs records ctx.Err() observed at the start of each Run call so a
	// test can assert which calls saw a live context and which saw a
	// canceled one.
	ctxErrs []error
}

func (f *fakeDockerClient) Run(ctx context.Context, inv docker.Invocation) (docker.CommandResult, error) {
	f.calls++
	f.invocationTypes = append(f.invocationTypes, fmt.Sprintf("%T", inv))
	f.invocationDetails = append(f.invocationDetails, fmt.Sprintf("%+v", inv))
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	if f.honorCtx {
		if err := ctx.Err(); err != nil {
			return docker.CommandResult{}, types.WrapError(
				types.ErrCodeUserCanceled,
				"docker command canceled",
				"",
				err,
			)
		}
	}
	if f.runFn != nil {
		return f.runFn(f.calls, inv)
	}
	return docker.CommandResult{}, nil
}

// StreamLogs implements docker.LogStreamer so Logs tests can feed raw
// compose-logs lines through the real wrapper parsing layer.
func (f *fakeDockerClient) StreamLogs(
	ctx context.Context,
	inv docker.Invocation,
	sink docker.RawLogSink,
) error {
	f.streamCalls++
	f.invocationTypes = append(f.invocationTypes, fmt.Sprintf("%T", inv))
	if f.streamFn != nil {
		return f.streamFn(ctx, inv, sink)
	}
	return nil
}

func fakeDockerClientFactory(client docker.Client) func(security.Redactor) (docker.Client, error) {
	return func(security.Redactor) (docker.Client, error) {
		return client, nil
	}
}

// fakeConfirmer records confirmation payloads and defaults to
// authorizing the prompt.
type fakeConfirmer struct {
	confirmFn func(ctx context.Context, c types.Confirmation) (bool, error)
	calls     []types.Confirmation
}

func (f *fakeConfirmer) Confirm(ctx context.Context, c types.Confirmation) (bool, error) {
	f.calls = append(f.calls, c)
	if f.confirmFn != nil {
		return f.confirmFn(ctx, c)
	}
	return true, nil
}

// missingNetworkResult mimics `docker network inspect` output for an
// absent network so EnsureNetwork takes its create path.
func missingNetworkResult(name string) (docker.CommandResult, error) {
	return docker.CommandResult{
		Stderr:   "Error: No such network: " + name,
		ExitCode: 1,
	}, errors.New("exit status 1")
}

func stepIndex(t *testing.T, steps []string, step string) int {
	t.Helper()

	for i, s := range steps {
		if s == step {
			return i
		}
	}
	t.Fatalf("step %q not emitted in %v", step, steps)
	return -1
}

func freeLocalTCPPort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func assertUsageValidation(t *testing.T, err error) {
	t.Helper()
	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeUsageValidation, typedErr.Code)
}

func assertVerificationFailed(t *testing.T, err error) {
	t.Helper()
	var typedErr *types.Error
	require.ErrorAs(t, err, &typedErr)
	assert.Equal(t, types.ErrCodeVerificationFailed, typedErr.Code)
}

func assertErrorChainDoesNotContain(t *testing.T, err error, forbidden string) {
	t.Helper()
	seen := map[error]struct{}{}
	var walk func(error)
	walk = func(current error) {
		t.Helper()
		if current == nil {
			return
		}
		if _, ok := seen[current]; ok {
			return
		}
		seen[current] = struct{}{}
		assert.NotContains(t, current.Error(), forbidden)
		walk(errors.Unwrap(current))
	}
	walk(err)
}

func fileModePerm(t *testing.T, path string) os.FileMode {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Mode().Perm()
}

const gibibyte = uint64(1024 * 1024 * 1024)
