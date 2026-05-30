/*
Copyright 2026 The Hearth Authors.

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

// Package nvidia is the v0 backend adapter for NVIDIA GPUs running NVIDIA-vLLM.
package nvidia

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	servingv1alpha1 "github.com/hearth-project/hearth/api/v1alpha1"
	"github.com/hearth-project/hearth/internal/backend"
)

// Vendor is the key under which this adapter registers.
const Vendor = "nvidia"

// Adapter renders NVIDIA-vLLM serving artifacts.
type Adapter struct{}

// New returns the NVIDIA adapter.
func New() *Adapter { return &Adapter{} }

var _ backend.BackendAdapter = (*Adapter)(nil)

func (a *Adapter) Vendor() string { return Vendor }

// PodSpec builds the serving container (image, rendered args/env, ports, probes,
// resources, shared-memory volume). Accelerator resources and scheduling are applied
// by the builder via Accelerator.
func (a *Adapter) PodSpec(svc *servingv1alpha1.LLMService, rt *servingv1alpha1.InferenceRuntime, m backend.ResolvedModel) (corev1.PodSpec, error) {
	data := backend.TemplateData{
		Model:   backend.ModelData{Path: m.Path},
		Service: backend.ServiceData{Name: svc.Name, Namespace: svc.Namespace},
	}

	args := append(append([]string{}, rt.Spec.Container.Args...), svc.Spec.Runtime.ArgsOverride...)
	renderedArgs, err := backend.RenderAll(args, data)
	if err != nil {
		return corev1.PodSpec{}, err
	}

	env, err := renderEnv(rt.Spec.Container.Env, data)
	if err != nil {
		return corev1.PodSpec{}, err
	}
	env = append(env, m.Env...)

	port := rt.Spec.Container.Port
	container := corev1.Container{
		Name:  backend.ServingContainerName,
		Image: rt.Spec.Container.Image,
		Args:  renderedArgs,
		Env:   env,
		Ports: []corev1.ContainerPort{{
			Name:          port.Name,
			ContainerPort: port.ContainerPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Resources:      computeResources(svc),
		ReadinessProbe: rt.Spec.Health.Readiness.DeepCopy(),
		LivenessProbe:  rt.Spec.Health.Liveness.DeepCopy(),
		StartupProbe:   rt.Spec.Health.Startup.DeepCopy(),
		// vLLM needs a large /dev/shm; the default 64Mi causes crashes under load.
		VolumeMounts: []corev1.VolumeMount{{Name: "dshm", MountPath: "/dev/shm"}},
	}

	pod := corev1.PodSpec{
		Containers: []corev1.Container{container},
		Volumes: []corev1.Volume{{
			Name:         "dshm",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}},
		}},
	}
	if gp := rt.Spec.Lifecycle.TerminationGracePeriodSeconds; gp != nil {
		pod.TerminationGracePeriodSeconds = gp
	}
	return pod, nil
}

// Accelerator maps the abstract request onto the device-plugin resource named by the
// runtime. v0 supports whole devices; fractional sharing (NVIDIA+HAMi) arrives later.
func (a *Adapter) Accelerator(svc *servingv1alpha1.LLMService, rt *servingv1alpha1.InferenceRuntime) (backend.AcceleratorRequest, error) {
	name := rt.Spec.Accelerator.ResourceName
	if name == "" {
		return backend.AcceleratorRequest{}, fmt.Errorf("runtime %q has no accelerator.resourceName", rt.Name)
	}
	count := svc.Spec.Resources.Accelerators
	if count <= 0 {
		count = 1
	}
	return backend.AcceleratorRequest{
		Resources: corev1.ResourceList{
			corev1.ResourceName(name): *resource.NewQuantity(int64(count), resource.DecimalSI),
		},
		NodeSelector:  rt.Spec.Accelerator.NodeSelector,
		Tolerations:   rt.Spec.Accelerator.Tolerations,
		SchedulerName: rt.Spec.Accelerator.Scheduler.Name,
	}, nil
}

func (a *Adapter) MetricsSource(rt *servingv1alpha1.InferenceRuntime) backend.MetricsSource {
	return backend.MetricsSource{
		Path:        rt.Spec.Metrics.Path,
		PortName:    rt.Spec.Metrics.Port,
		QueueDepth:  rt.Spec.Metrics.QueueDepth,
		KVCacheUtil: rt.Spec.Metrics.KVCacheUtil,
	}
}

func computeResources(svc *servingv1alpha1.LLMService) corev1.ResourceRequirements {
	r := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	if cpu := svc.Spec.Resources.CPU; cpu != nil {
		r.Requests[corev1.ResourceCPU] = *cpu
	}
	if mem := svc.Spec.Resources.Memory; mem != nil {
		r.Requests[corev1.ResourceMemory] = *mem
		r.Limits[corev1.ResourceMemory] = *mem
	}
	if len(r.Requests) == 0 {
		r.Requests = nil
	}
	if len(r.Limits) == 0 {
		r.Limits = nil
	}
	return r
}

func renderEnv(in []corev1.EnvVar, data backend.TemplateData) ([]corev1.EnvVar, error) {
	out := make([]corev1.EnvVar, 0, len(in))
	for _, e := range in {
		if e.Value != "" {
			v, err := backend.Render(e.Value, data)
			if err != nil {
				return nil, err
			}
			e.Value = v
		}
		out = append(out, e)
	}
	return out, nil
}
