/*
Copyright 2018 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resources

import (
	"fmt"
	"strconv"

	network "knative.dev/networking/pkg"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/ptr"
	"knative.dev/serving/pkg/apis/autoscaling"
	v1 "knative.dev/serving/pkg/apis/serving/v1"
	"knative.dev/serving/pkg/networking"
	"knative.dev/serving/pkg/queue"
	"knative.dev/serving/pkg/reconciler/revision/config"
	"knative.dev/serving/pkg/reconciler/revision/resources/names"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

//nolint:gosec // Filepath, not hardcoded credentials
const concurrencyStateTokenVolumeMountPath = "/var/run/secrets/tokens"
const concurrencyStateTokenName = "state-token"

var (
	varLogVolume = corev1.Volume{
		Name: "knative-var-log",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}

	varLogVolumeMount = corev1.VolumeMount{
		Name:        varLogVolume.Name,
		MountPath:   "/var/log",
		SubPathExpr: "$(K_INTERNAL_POD_NAMESPACE)_$(K_INTERNAL_POD_NAME)_",
	}

	varTokenVolume = corev1.Volume{
		Name: "knative-token-volume",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						ExpirationSeconds: ptr.Int64(600),
						Path:              concurrencyStateTokenName,
						Audience:          "concurrency-state-hook"},
				}},
			},
		},
	}

	varTokenVolumeMount = corev1.VolumeMount{
		Name:      varTokenVolume.Name,
		MountPath: concurrencyStateTokenVolumeMountPath,
	}

	// This PreStop hook is actually calling an endpoint on the queue-proxy
	// because of the way PreStop hooks are called by kubelet. We use this
	// to block the user-container from exiting before the queue-proxy is ready
	// to exit so we can guarantee that there are no more requests in flight.
	userLifecycle = &corev1.Lifecycle{
		PreStop: &corev1.Handler{
			HTTPGet: &corev1.HTTPGetAction{
				Port: intstr.FromInt(networking.QueueAdminPort),
				Path: queue.RequestQueueDrainPath,
			},
		},
	}
)

func rewriteUserProbe(p *corev1.Probe, userPort int) {
	if p == nil {
		return
	}
	switch {
	case p.HTTPGet != nil:
		p.HTTPGet.Port = intstr.FromInt(userPort)
		// With mTLS enabled, Istio rewrites probes, but doesn't spoof the kubelet
		// user agent, so we need to inject an extra header to be able to distinguish
		// between probes and real requests.
		p.HTTPGet.HTTPHeaders = append(p.HTTPGet.HTTPHeaders, corev1.HTTPHeader{
			Name:  network.KubeletProbeHeaderName,
			Value: queue.Name,
		})
	case p.TCPSocket != nil:
		p.TCPSocket.Port = intstr.FromInt(userPort)
	}
}

func makePodSpec(rev *v1.Revision, cfg *config.Config) (*corev1.PodSpec, error) {
	queueContainer, err := makeQueueContainer(rev, cfg)

	if err != nil {
		return nil, fmt.Errorf("failed to create queue-proxy container: %w", err)
	}

	var extraVolumes []corev1.Volume
	// If concurrencyStateEndpoint is enabled, add the serviceAccountToken to QP via a projected volume
	if cfg.Deployment.ConcurrencyStateEndpoint != "" {
		queueContainer.VolumeMounts = append(queueContainer.VolumeMounts, varTokenVolumeMount)
		extraVolumes = append(extraVolumes, varTokenVolume)
	}

	podSpec := BuildPodSpec(rev, append(BuildUserContainers(rev), *queueContainer), cfg)
	podSpec.Volumes = append(podSpec.Volumes, extraVolumes...)

	if cfg.Observability.EnableVarLogCollection {
		podSpec.Volumes = append(podSpec.Volumes, varLogVolume)

		for i, container := range podSpec.Containers {
			if container.Name == QueueContainerName {
				continue
			}

			varLogMount := varLogVolumeMount.DeepCopy()
			varLogMount.SubPathExpr += container.Name
			container.VolumeMounts = append(container.VolumeMounts, *varLogMount)
			container.Env = append(container.Env, buildVarLogSubpathEnvs()...)

			podSpec.Containers[i] = container
		}
	}

	return podSpec, nil
}

// BuildUserContainers makes an array of containers from the Revision template.
func BuildUserContainers(rev *v1.Revision) []corev1.Container {
	containers := make([]corev1.Container, 0, len(rev.Spec.PodSpec.Containers))
	for i := range rev.Spec.PodSpec.Containers {
		var container corev1.Container
		if len(rev.Spec.PodSpec.Containers[i].Ports) != 0 || len(rev.Spec.PodSpec.Containers) == 1 {
			container = makeServingContainer(*rev.Spec.PodSpec.Containers[i].DeepCopy(), rev)
		} else {
			container = makeContainer(*rev.Spec.PodSpec.Containers[i].DeepCopy(), rev)
		}
		// The below logic is safe because the image digests in Status.ContainerStatus will have been resolved
		// before this method is called. We check for an empty array here because the method can also be
		// called during DryRun, where ContainerStatuses will not yet have been resolved.
		if len(rev.Status.ContainerStatuses) != 0 {
			if rev.Status.ContainerStatuses[i].ImageDigest != "" {
				container.Image = rev.Status.ContainerStatuses[i].ImageDigest
			}
		}
		containers = append(containers, container)
	}
	return containers
}

func makeContainer(container corev1.Container, rev *v1.Revision) corev1.Container {
	// Adding or removing an overwritten corev1.Container field here? Don't forget to
	// update the fieldmasks / validations in pkg/apis/serving
	container.Lifecycle = userLifecycle
	container.Env = append(container.Env, getKnativeEnvVar(rev)...)

	// Explicitly disable stdin and tty allocation
	container.Stdin = false
	container.TTY = false
	if container.TerminationMessagePolicy == "" {
		container.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	}
	return container
}

func makeServingContainer(servingContainer corev1.Container, rev *v1.Revision) corev1.Container {
	userPort := getUserPort(rev)
	userPortStr := strconv.Itoa(int(userPort))
	// Replacement is safe as only up to a single port is allowed on the Revision
	servingContainer.Ports = buildContainerPorts(userPort)
	servingContainer.Env = append(servingContainer.Env, buildUserPortEnv(userPortStr))
	container := makeContainer(servingContainer, rev)
	if container.ReadinessProbe != nil {
		if container.ReadinessProbe.HTTPGet != nil || container.ReadinessProbe.TCPSocket != nil {
			// HTTP and TCP ReadinessProbes are executed by the queue-proxy directly against the
			// user-container instead of via kubelet.
			container.ReadinessProbe = nil
		}
	}
	// If the client provides probes, we should fill in the port for them.
	rewriteUserProbe(container.LivenessProbe, int(userPort))
	return container
}

// BuildPodSpec creates a PodSpec from the given revision and containers.
// cfg can be passed as nil if not within revision reconciliation context.
func BuildPodSpec(rev *v1.Revision, containers []corev1.Container, cfg *config.Config) *corev1.PodSpec {
	pod := rev.Spec.PodSpec.DeepCopy()
	pod.Containers = containers
	pod.TerminationGracePeriodSeconds = rev.Spec.TimeoutSeconds
	if cfg != nil && pod.EnableServiceLinks == nil {
		pod.EnableServiceLinks = cfg.Defaults.EnableServiceLinks
	}
	return pod
}

func getUserPort(rev *v1.Revision) int32 {
	ports := rev.Spec.GetContainer().Ports

	if len(ports) > 0 && ports[0].ContainerPort != 0 {
		return ports[0].ContainerPort
	}

	return v1.DefaultUserPort
}

func buildContainerPorts(userPort int32) []corev1.ContainerPort {
	return []corev1.ContainerPort{{
		Name:          v1.UserPortName,
		ContainerPort: userPort,
	}}
}

func buildVarLogSubpathEnvs() []corev1.EnvVar {
	return []corev1.EnvVar{{
		Name: "K_INTERNAL_POD_NAME",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "metadata.name",
			},
		},
	}, {
		Name: "K_INTERNAL_POD_NAMESPACE",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{
				FieldPath: "metadata.namespace",
			},
		},
	}}
}

func buildUserPortEnv(userPort string) corev1.EnvVar {
	return corev1.EnvVar{
		Name:  "PORT",
		Value: userPort,
	}
}

// MakeDeployment constructs a K8s Deployment resource from a revision.
func MakeDeployment(rev *v1.Revision, cfg *config.Config) (*appsv1.Deployment, error) {
	podSpec, err := makePodSpec(rev, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create PodSpec: %w", err)
	}

	replicaCount := cfg.Autoscaler.InitialScale
	ann, found := rev.Annotations[autoscaling.InitialScaleAnnotationKey]
	if found {
		// Ignore errors and no error checking because already validated in webhook.
		rc, _ := strconv.ParseInt(ann, 10, 32)
		replicaCount = int32(rc)
	}

	labels := makeLabels(rev)
	anns := makeAnnotations(rev)

	// Slowly but steadily roll the deployment out, to have the least possible impact.
	maxUnavailable := intstr.FromInt(0)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            names.Deployment(rev),
			Namespace:       rev.Namespace,
			Labels:          labels,
			Annotations:     anns,
			OwnerReferences: []metav1.OwnerReference{*kmeta.NewControllerRef(rev)},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:                ptr.Int32(replicaCount),
			Selector:                makeSelector(rev),
			ProgressDeadlineSeconds: ptr.Int32(int32(cfg.Deployment.ProgressDeadline.Seconds())),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &maxUnavailable,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: anns,
				},
				Spec: *podSpec,
			},
		},
	}, nil
}
