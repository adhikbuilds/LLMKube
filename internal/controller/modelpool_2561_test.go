/*
Copyright 2026.

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

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// #2561: spec.swapDelay counts toward spec.swapBudget, so the API server
// rejects a delay at or above the budget, which would let no held request
// ever start a swap.
var _ = Describe("ModelPool #2561 swapDelay validation", func() {
	ctx := context.Background()
	const poolName = "repro-2561"

	AfterEach(func() {
		p := &inferencev1alpha1.ModelPool{ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"}}
		_ = k8sClient.Delete(ctx, p)
	})

	dur := func(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

	DescribeTable("creating a pool",
		func(delay, budget *metav1.Duration, wantErr string) {
			err := k8sClient.Create(ctx, &inferencev1alpha1.ModelPool{
				ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
				Spec: inferencev1alpha1.ModelPoolSpec{
					SwapBudget: budget,
					SwapDelay:  delay,
					Members: []inferencev1alpha1.ModelPoolMember{
						{InferenceServiceRef: corev1.LocalObjectReference{Name: poolName + "-a"}},
					},
				},
			})
			if wantErr == "" {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(wantErr))
		},
		Entry("accepts a delay shorter than the budget", dur(5*time.Second), dur(300*time.Second), ""),
		Entry("accepts a zero delay", dur(0), dur(300*time.Second), ""),
		Entry("accepts no delay", nil, dur(300*time.Second), ""),
		Entry("rejects a delay equal to the budget", dur(60*time.Second), dur(60*time.Second),
			"swapDelay must be shorter than swapBudget"),
		Entry("rejects a delay longer than the budget", dur(10*time.Minute), dur(300*time.Second),
			"swapDelay must be shorter than swapBudget"),
		Entry("rejects a delay at the defaulted budget", dur(300*time.Second), nil,
			"swapDelay must be shorter than swapBudget"),
		Entry("rejects a negative delay", dur(-time.Second), dur(300*time.Second),
			"swapDelay must not be negative"),
	)
})
