/*
Copyright 2022 The Koordinator Authors.

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

package validating

import (
	"context"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientcache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/runtime/inject"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling/v1alpha1"

	"github.com/koordinator-sh/koordinator/pkg/webhook/elasticquota"
)

// PodValidatingHandler handles Pod
type PodValidatingHandler struct {
	Client client.Client

	// Decoder decodes objects
	Decoder *admission.Decoder
}

var _ admission.Handler = &PodValidatingHandler{}

func shouldIgnoreIfNotPod(req admission.Request) bool {
	// Ignore all calls to sub resources or resources other than pods.
	if len(req.AdmissionRequest.SubResource) != 0 ||
		req.AdmissionRequest.Resource.Resource != "pods" {
		return true
	}
	return false
}

func (h *PodValidatingHandler) validatingPodFn(ctx context.Context, req admission.Request) (allowed bool, reason string, err error) {
	allowed = true
	if shouldIgnoreIfNotPod(req) {
		return
	}
	if req.Operation == admissionv1.Delete && len(req.OldObject.Raw) == 0 {
		klog.Warningf("Skip to validate pod %s/%s deletion for no old object, maybe because of Kubernetes version < 1.16", req.Namespace, req.Name)
		return
	}

	allowed, reason, err = h.clusterColocationProfileValidatingPod(ctx, req)
	if err == nil {
		plugin := elasticquota.NewPlugin(h.Decoder, h.Client)
		if err = plugin.ValidatePod(ctx, req); err != nil {
			return false, "", err
		}
	}
	return
}

var _ admission.Handler = &PodValidatingHandler{}

// Handle handles admission requests.
func (h *PodValidatingHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	allowed, reason, err := h.validatingPodFn(ctx, req)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	return admission.ValidationResponse(allowed, reason)
}

var _ inject.Client = &PodValidatingHandler{}

// InjectClient injects the client into the PodValidatingHandler
func (h *PodValidatingHandler) InjectClient(c client.Client) error {
	h.Client = c
	return nil
}

var _ admission.DecoderInjector = &PodValidatingHandler{}

// InjectDecoder injects the decoder into the PodValidatingHandler
func (h *PodValidatingHandler) InjectDecoder(d *admission.Decoder) error {
	h.Decoder = d
	return nil
}

var _ inject.Cache = &PodValidatingHandler{}

func (h *PodValidatingHandler) InjectCache(cache cache.Cache) error {
	ctx := context.TODO()
	quotaInformer, err := cache.GetInformer(ctx, &v1alpha1.ElasticQuota{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ElasticQuota",
			APIVersion: v1alpha1.SchemeGroupVersion.String(),
		},
	})
	if err != nil {
		return err
	}
	plugin := elasticquota.NewPlugin(h.Decoder, h.Client)
	qt := plugin.QuotaTopo
	quotaInformer.AddEventHandler(clientcache.ResourceEventHandlerFuncs{
		AddFunc:    qt.OnQuotaAdd,
		UpdateFunc: qt.OnQuotaUpdate,
		DeleteFunc: qt.OnQuotaDelete,
	})

	sharedInformer := quotaInformer.(clientcache.SharedIndexInformer)

	go sharedInformer.Run(ctx.Done())
	clientcache.WaitForCacheSync(ctx.Done(), sharedInformer.HasSynced)
	return nil
}
