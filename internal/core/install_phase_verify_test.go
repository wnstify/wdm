package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wnstify/wdm/internal/catalog"
	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/internal/render"
	"github.com/wnstify/wdm/pkg/types"
)

// fuseInstallStatus is the pure verify phase (issue #121): it fuses the
// inspected container set into the reported app status with no Docker
// dependency, so it is table-tested directly with literal inputs rather
// than only through the full Install path. The plan carries just the four
// fields the fusion reads — app id, completed-by-design services, the
// rendered service names, and the planned local ports.

func fuseTestPlan(appID string, services, completed []string, localPorts []types.PortBinding) *installPlan {
	serviceLabels := make(map[string]map[string]string, len(services))
	for _, service := range services {
		serviceLabels[service] = map[string]string{}
	}
	return &installPlan{
		app:        catalog.App{AppID: appID, CompletedServices: completed},
		localPorts: localPorts,
		rendered:   render.RenderedStack{ServiceLabels: serviceLabels},
	}
}

func managedContainer(appID, service string, state docker.ContainerState, ports ...docker.PublishedPort) docker.ContainerInfo {
	return docker.ContainerInfo{
		Name:    service + "-1",
		Service: service,
		Labels:  map[string]string{"wdm.managed": "true", "wdm.app": appID},
		State:   state,
		Ports:   ports,
	}
}

func TestFuseInstallStatus(t *testing.T) {
	t.Parallel()

	const appID = "demo"
	running := docker.ContainerState{Status: "running", Running: true}

	tests := []struct {
		name             string
		services         []string
		completed        []string
		localPorts       []types.PortBinding
		containers       []docker.ContainerInfo
		wantState        string
		wantAttention    bool
		wantReasons      []string
		wantServiceState map[string]string
	}{
		{
			name:          "all services running",
			services:      []string{"web", "db"},
			containers:    []docker.ContainerInfo{managedContainer(appID, "web", running), managedContainer(appID, "db", running)},
			wantState:     statusStateRunning,
			wantAttention: false,
			wantServiceState: map[string]string{
				"web": statusStateRunning,
				"db":  statusStateRunning,
			},
		},
		{
			name:          "missing managed container",
			services:      []string{"web", "db"},
			containers:    []docker.ContainerInfo{managedContainer(appID, "web", running)},
			wantState:     statusStateNeedsAttention,
			wantAttention: true,
			wantReasons:   []string{statusReasonContainerMissing},
			wantServiceState: map[string]string{
				"web": statusStateRunning,
				"db":  "missing",
			},
		},
		{
			name:       "planned port not published",
			services:   []string{"web"},
			localPorts: []types.PortBinding{{Service: "web", HostPort: 8080, Protocol: "tcp"}},
			containers: []docker.ContainerInfo{
				managedContainer(appID, "web", running, docker.PublishedPort{HostPort: 9090, Protocol: "tcp"}),
			},
			wantState:     statusStateNeedsAttention,
			wantAttention: true,
			wantReasons:   []string{statusReasonPortMismatch},
		},
		{
			name:       "planned port published",
			services:   []string{"web"},
			localPorts: []types.PortBinding{{Service: "web", HostPort: 8080, Protocol: "tcp"}},
			containers: []docker.ContainerInfo{
				managedContainer(appID, "web", running, docker.PublishedPort{HostPort: 8080, Protocol: "tcp"}),
			},
			wantState:     statusStateRunning,
			wantAttention: false,
		},
		{
			name:      "completed by design exits clean",
			services:  []string{"init"},
			completed: []string{"init"},
			containers: []docker.ContainerInfo{
				managedContainer(appID, "init", docker.ContainerState{Status: "exited", Running: false, ExitCode: 0}),
			},
			wantState:        statusStateRunning,
			wantAttention:    false,
			wantServiceState: map[string]string{"init": statusStateCompleted},
		},
		{
			name:     "restart loop needs attention",
			services: []string{"web"},
			containers: []docker.ContainerInfo{
				managedContainer(appID, "web", docker.ContainerState{Status: "restarting", Running: false, Restarting: true}),
			},
			wantState:     statusStateNeedsAttention,
			wantAttention: true,
			wantReasons:   []string{statusReasonRestartLoop},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			plan := fuseTestPlan(appID, tt.services, tt.completed, tt.localPorts)
			status := &types.AppStatus{AppID: appID}
			fuseInstallStatus(plan, tt.containers, status)

			assert.Equal(t, tt.wantState, status.State)
			assert.Equal(t, tt.wantAttention, status.NeedsAttention)
			for _, reason := range tt.wantReasons {
				assert.Contains(t, status.AttentionReasons, reason)
			}
			if !tt.wantAttention {
				assert.Empty(t, status.AttentionReasons)
			}
			for service, wantState := range tt.wantServiceState {
				found := false
				for _, svc := range status.Services {
					if svc.Service == service {
						found = true
						assert.Equal(t, wantState, svc.State, "service %q state", service)
					}
				}
				require.True(t, found, "service %q must appear in fused status", service)
			}
		})
	}
}

// TestFuseInstallStatus_DriftedLabelsCountAsMissing proves a project
// container whose wdm ownership labels are absent is not counted as a
// managed service: the expected service surfaces as missing, the verify
// phase fails closed rather than trusting an unlabeled container.
func TestFuseInstallStatus_DriftedLabelsCountAsMissing(t *testing.T) {
	t.Parallel()

	const appID = "demo"
	plan := fuseTestPlan(appID, []string{"web"}, nil, nil)
	unlabeled := docker.ContainerInfo{
		Name:    "web-1",
		Service: "web",
		Labels:  map[string]string{},
		State:   docker.ContainerState{Status: "running", Running: true},
	}
	status := &types.AppStatus{AppID: appID}
	fuseInstallStatus(plan, []docker.ContainerInfo{unlabeled}, status)

	assert.Equal(t, statusStateNeedsAttention, status.State)
	assert.True(t, status.NeedsAttention)
	assert.Contains(t, status.AttentionReasons, statusReasonContainerMissing)
}
