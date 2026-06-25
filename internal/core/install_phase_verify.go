package core

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/wnstify/wdm/internal/docker"
	"github.com/wnstify/wdm/pkg/types"
)

// verifyInstallStatus inspects the deployed containers by Compose
// project and wdm labels and fuses the install-time PRD §18
// conditions (missing container, unexpected exit, restart loop,
// unhealthy, port mismatch) into a [types.AppStatus]. The pass runs
// AFTER the protocol step 6 commit point, so a failed inspection never
// fails the install: it marks the result needs-attention with the
// status_check_failed reason instead. Context cancellation propagates
// — the durable manifest stays in place either way.
func verifyInstallStatus(
	ctx context.Context,
	client docker.Client,
	plan *installPlan,
	onProgress types.ProgressFn,
) (*types.AppStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(types.StepInstallStatus, 90, "verifying deployed stack status")
	}

	now := time.Now().UTC()
	status := &types.AppStatus{
		AppID:          plan.app.AppID,
		ComposeProject: plan.composeProject,
		StackPath:      plan.stackPath,
		LocalPorts:     append([]types.PortBinding(nil), plan.localPorts...),
		UpdatedAt:      &now,
	}

	containers, err := docker.InspectProjectContainers(ctx, client, plan.composeProject)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		status.State = statusStateNeedsAttention
		status.NeedsAttention = true
		status.AttentionReasons = []string{statusReasonStatusCheckFailed}
		status.Message = "post-install status verification failed; run apps status for details"
		return status, nil
	}

	fuseInstallStatus(plan, containers, status)
	return status, nil
}

// fuseInstallStatus matches the expected rendered services against
// the inspected containers through the shared
// [fuseManagedServiceStatuses] fusion (PRD §10, §18; row
// 33), then layers the install-specific port check on top: the plan
// carries full bindings, so a planned port counts as published only
// when both the protocol and the host port match.
func fuseInstallStatus(plan *installPlan, containers []docker.ContainerInfo, status *types.AppStatus) {
	services := make([]string, 0, len(plan.rendered.ServiceLabels))
	for service := range plan.rendered.ServiceLabels {
		services = append(services, service)
	}
	sort.Strings(services)

	managed, reasons := fuseManagedServiceStatuses(plan.app.AppID, services, completedServiceSet(plan.app.CompletedServices), containers, status)

	published := map[string]struct{}{}
	for _, container := range managed {
		for _, port := range container.Ports {
			published[fmt.Sprintf("%s/%d", port.Protocol, port.HostPort)] = struct{}{}
		}
	}
	for _, binding := range plan.localPorts {
		if _, ok := published[fmt.Sprintf("%s/%d", binding.Protocol, binding.HostPort)]; !ok {
			reasons[statusReasonPortMismatch] = struct{}{}
		}
	}

	finalizeStatus(
		status,
		reasons,
		"all managed services are running",
		"post-install verification found issues; run apps status for details",
	)
}

func publishedPortBindings(container docker.ContainerInfo) []types.PortBinding {
	if len(container.Ports) == 0 {
		return nil
	}
	bindings := make([]types.PortBinding, 0, len(container.Ports))
	for _, port := range container.Ports {
		bindings = append(bindings, types.PortBinding{
			Service:       container.Service,
			HostIP:        port.HostIP,
			HostPort:      port.HostPort,
			ContainerPort: port.ContainerPort,
			Protocol:      port.Protocol,
		})
	}
	return bindings
}

// buildInstallResult assembles the structured install result (PRD §17
// steps 13-14, §32): Compose project, started services, local ports,
// the post-install guidance built at render time, and the fused
// post-deploy status snapshot.
func buildInstallResult(plan *installPlan, status *types.AppStatus) *types.InstallResult {
	var started []string
	for _, service := range status.Services {
		if service.State == statusStateRunning {
			started = append(started, service.Service)
		}
	}
	sort.Strings(started)

	return &types.InstallResult{
		AppID:               plan.app.AppID,
		StackPath:           plan.stackPath,
		ComposeProject:      plan.composeProject,
		StartedServices:     started,
		LocalPorts:          append([]types.PortBinding(nil), plan.localPorts...),
		PostInstallGuidance: plan.guidance,
		Status:              status,
	}
}
