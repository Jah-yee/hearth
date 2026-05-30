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

package backend_test

import (
	"testing"

	. "github.com/onsi/gomega"

	"github.com/hearth-project/hearth/internal/backend"
)

func TestServiceMonitorScrapesHTTPMetrics(t *testing.T) {
	g := NewWithT(t)
	sm := backend.BuildServiceMonitor(scalingService())

	g.Expect(sm.GetAPIVersion()).To(Equal("monitoring.coreos.com/v1"))
	g.Expect(sm.GetKind()).To(Equal("ServiceMonitor"))

	spec := sm.Object["spec"].(map[string]any)
	matchLabels := spec["selector"].(map[string]any)["matchLabels"].(map[string]any)
	g.Expect(matchLabels).To(HaveKeyWithValue("serving.hearth.dev/llmservice", "qwen3-8b"))

	endpoints := spec["endpoints"].([]any)
	g.Expect(endpoints).To(HaveLen(1))
	ep := endpoints[0].(map[string]any)
	g.Expect(ep["port"]).To(Equal("http"))
	g.Expect(ep["path"]).To(Equal("/metrics"))
}
