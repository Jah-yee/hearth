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

// Package backend defines the vendor-neutral abstraction that lets one LLMService
// run on any vLLM runtime (NVIDIA / Ascend / MLU). Adapters only do K8s-layer
// adaptation — scheduling, health, model loading, metrics — never chip kernels.
package backend

import (
	"bytes"
	"fmt"
	"text/template"

	corev1 "k8s.io/api/core/v1"

	servingv1alpha1 "github.com/hearth-project/hearth/api/v1alpha1"
)

// ResolvedModel is the outcome of resolving spec.model into something a runtime
// can load: a path/identifier plus any env required to fetch it.
type ResolvedModel struct {
	// Path is passed to the runtime as the model to serve (a repo id for now;
	// a local cache path once caching lands).
	Path string

	// Source is the registry the model is fetched from ("hf" or "modelscope"); it
	// selects the prewarm download command.
	Source string

	// Env is extra environment required to load the model (e.g. VLLM_USE_MODELSCOPE).
	Env []corev1.EnvVar
}

// AcceleratorRequest is the K8s-level accelerator and scheduling derived from a runtime.
type AcceleratorRequest struct {
	Resources     corev1.ResourceList
	NodeSelector  map[string]string
	Tolerations   []corev1.Toleration
	SchedulerName string
}

// MetricsSource tells the scaler where a runtime exposes its LLM-aware signals.
type MetricsSource struct {
	Path        string
	PortName    string
	QueueDepth  string
	KVCacheUtil string
}

// BackendAdapter renders the K8s artifacts for one vendor runtime.
type BackendAdapter interface {
	Vendor() string
	PodSpec(svc *servingv1alpha1.LLMService, rt *servingv1alpha1.InferenceRuntime, m ResolvedModel) (corev1.PodSpec, error)
	Accelerator(svc *servingv1alpha1.LLMService, rt *servingv1alpha1.InferenceRuntime) (AcceleratorRequest, error)
	MetricsSource(rt *servingv1alpha1.InferenceRuntime) MetricsSource
}

// Registry maps a vendor key to its adapter; new chips are new entries.
type Registry struct {
	adapters map[string]BackendAdapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{adapters: map[string]BackendAdapter{}}
}

// Register adds (or replaces) the adapter for its vendor.
func (r *Registry) Register(a BackendAdapter) {
	r.adapters[a.Vendor()] = a
}

// Get returns the adapter for a vendor.
func (r *Registry) Get(vendor string) (BackendAdapter, bool) {
	a, ok := r.adapters[vendor]
	return a, ok
}

// TemplateData is the context available to InferenceRuntime arg/env templates.
type TemplateData struct {
	Model       ModelData
	Service     ServiceData
	Accelerator AcceleratorData
}

// ModelData exposes the resolved model to templates.
type ModelData struct{ Path string }

// ServiceData exposes the LLMService identity to templates.
type ServiceData struct{ Name, Namespace string }

// AcceleratorData exposes accelerator hints (e.g. visible-device index) to templates.
type AcceleratorData struct{ Index string }

// Render expands a single Go-template string against data.
func Render(tmpl string, data TemplateData) (string, error) {
	t, err := template.New("tmpl").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template %q: %w", tmpl, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render template %q: %w", tmpl, err)
	}
	return buf.String(), nil
}

// RenderAll expands a slice of template strings.
func RenderAll(tmpls []string, data TemplateData) ([]string, error) {
	out := make([]string, 0, len(tmpls))
	for _, s := range tmpls {
		r, err := Render(s, data)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
