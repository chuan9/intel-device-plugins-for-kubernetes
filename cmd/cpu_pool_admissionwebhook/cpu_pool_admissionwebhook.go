// Copyright 2018 Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"

	"github.com/golang/glog"

	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// resourcePrefix is the resource namespace and prefix of extended resources for CPU pool allocation
	resourcePrefix = "intel.com/cpupool."

	// defaultPool has CPUs which are assigned to workloads which don't request specific pools
	defaultPool   = "default"
	addResourceOp = `{
                "op": "add",
                "path": "/spec/%s/%d/resources/%s/%s",
                "value": "%s"
		}`
)

var (
	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme)
)

type poolResource struct {
	quantity resource.Quantity
	pool     corev1.ResourceName
	system   bool
}

func init() {
	addToScheme(scheme)
}

func addToScheme(scheme *runtime.Scheme) {
	corev1.AddToScheme(scheme)
	admissionregistrationv1beta1.AddToScheme(scheme)
}

func getTLSConfig(certFile string, keyFile string) *tls.Config {
	sCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		glog.Fatal(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{sCert},
	}
}

func validatePoolResource(c *corev1.Container, request *poolResource, limit *poolResource) error {
	var poolRequest, poolLimit, cpuRequest, cpuLimit resource.Quantity

	var requestName string

	// For the validity check to pass we need to have:
	//  - no pool label, no CPU request, no pool request, or
	//  - no CPU request and a label-matching pool request with value 1, or
	//  - a CPU request and a label-matching pool request with equal value * 1000

	if c.Resources.Requests != nil {
		for name, res := range c.Resources.Requests {
			if name == corev1.ResourceCPU {
				cpuRequest = res
			} else if strings.HasPrefix(name.String(), resourcePrefix) {
				if poolRequest.MilliValue() != 0 {
					return fmt.Errorf("container %s: multiple CPU pools", c.Name)
				}
				poolRequest = res
				requestName = name.String()
			}
		}
	}

	if c.Resources.Limits != nil {
		for name, res := range c.Resources.Limits {
			if name == corev1.ResourceCPU {
				cpuLimit = res
			} else if strings.HasPrefix(name.String(), resourcePrefix) {
				if poolLimit.MilliValue() != 0 {
					return fmt.Errorf("container %s: multiple CPU pools", c.Name)
				}
				poolLimit = res

				if requestName != name.String() {
					return fmt.Errorf("container %s: limit and request asked from different pools", c.Name)
				}
			}
		}
	}

	if request != nil {
		if request.system {
			cpuRequest = request.quantity
		} else {
			poolRequest = request.quantity
		}
	}

	if poolRequest.Value() != cpuRequest.MilliValue() {
		return fmt.Errorf("container %s: inconsistent native (%d) vs. pool (%d) CPU requests", c.Name, cpuRequest.MilliValue(), poolRequest.Value())
	}

	if limit != nil {
		if request.system {
			cpuLimit = limit.quantity
		} else {
			poolLimit = limit.quantity
		}
	}

	if poolLimit.Value() != cpuLimit.MilliValue() {
		return fmt.Errorf("container %s: inconsistent native (%d) vs. pool (%d) CPU limits", c.Name, cpuLimit.MilliValue(), poolLimit.Value())
	}

	return nil
}

func addPoolResourceRequestOrLimit(c *corev1.Container, request bool) (*poolResource, error) {
	var pool, cpu *resource.Quantity = nil, nil
	list := c.Resources.Limits

	if (request && c.Resources.Requests == nil) || (!request && c.Resources.Limits == nil) {
		return nil, nil
	}

	if request {
		list = c.Resources.Requests
	}

	//
	// Find any native and pool CPU requests, then
	//
	// - if both present, do nothing (will be validated later)
	// - if pool present, add corresponding native
	// - if native present, add corresponding default pool
	//

	if res, ok := list[corev1.ResourceCPU]; ok {
		cpu = &res
	}

	for name, res := range list {
		if strings.HasPrefix(name.String(), resourcePrefix) {
			pool = &res
			break
		}
	}

	if cpu != nil && pool != nil {
		// both native and pool CPU request/limit found
		return nil, nil
	}

	if pool != nil {
		// only pool CPU request/limit, add native
		val := pool.Value()
		cpu = resource.NewMilliQuantity(val, resource.DecimalSI)

		return &poolResource{
			system:   true,
			quantity: *cpu,
		}, nil
	}

	if cpu == nil {
		// no pool and no native CPU means no need to edit anything
		return nil, nil
	}

	// only native CPU request/limit, add 'default' pool one

	val := cpu.MilliValue()
	pool = resource.NewQuantity(val, resource.DecimalSI)
	name := corev1.ResourceName(resourcePrefix + defaultPool)

	return &poolResource{
		system:   false,
		quantity: *pool,
		pool:     name,
	}, nil
}

// addPoolResource extends the given container with an extended resource request for a CPU pool.
func addPoolResource(c *corev1.Container) (*poolResource, *poolResource, error) {

	request, err := addPoolResourceRequestOrLimit(c, true)
	if err != nil {
		return nil, nil, err
	}
	limit, err := addPoolResourceRequestOrLimit(c, false)
	if err != nil {
		return nil, nil, err
	}

	return request, limit, nil
}

func escapeName(name string) string {
	str := strings.Replace(name, "~", "~0", -1)
	return strings.Replace(str, "/", "~1", -1)
}

func createOp(res *poolResource, i int, resourceType string, target string) string {
	resourceName := "cpu"

	if !res.system {
		resourceName = res.pool.String()
	}

	return fmt.Sprintf(addResourceOp, target, i, resourceType, escapeName(resourceName), res.quantity.String())
}

func mutatePods(ar v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	var ops []string

	glog.V(2).Info("mutating pods")

	podResource := metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	if ar.Request.Resource != podResource {
		glog.Errorf("expect resource to be %s", podResource)
		return toAdmissionResponse(fmt.Errorf("wrong resource type (%s, expected %s)", ar.Request.Resource, podResource))
	}

	raw := ar.Request.Object.Raw
	pod := corev1.Pod{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(raw, nil, &pod); err != nil {
		glog.Error(err)
		return toAdmissionResponse(err)
	}
	reviewResponse := v1beta1.AdmissionResponse{}
	reviewResponse.Allowed = true

	glog.V(2).Info("pod namespace: " + pod.ObjectMeta.GetNamespace())

	// leave system-pods alone, they're supposed to have enough reserved CPU on each node
	if pod.ObjectMeta.GetNamespace() == metav1.NamespaceSystem {
		return &reviewResponse
	}

	for i := range pod.Spec.InitContainers {
		request, limit, err := addPoolResource(&pod.Spec.InitContainers[i])
		if err != nil {
			return toAdmissionResponse(err)
		}
		if request != nil {
			ops = append(ops, createOp(request, i, "requests", "initcontainers"))
		}
		if limit != nil {
			ops = append(ops, createOp(limit, i, "limits", "initcontainers"))
		}
		if err := validatePoolResource(&pod.Spec.InitContainers[i], request, limit); err != nil {
			return toAdmissionResponse(err)
		}
	}

	for i := range pod.Spec.Containers {
		request, limit, err := addPoolResource(&pod.Spec.Containers[i])
		if err != nil {
			return toAdmissionResponse(err)
		}
		if request != nil {
			ops = append(ops, createOp(request, i, "requests", "containers"))
		}
		if limit != nil {
			ops = append(ops, createOp(limit, i, "limits", "containers"))
		}
		if err := validatePoolResource(&pod.Spec.Containers[i], request, limit); err != nil {
			return toAdmissionResponse(err)
		}
	}

	if len(ops) > 0 {
		str := "[ " + strings.Join(ops, ",") + " ]"
		glog.V(2).Infof("patch: %s", str)
		reviewResponse.Patch = []byte(str)
		pt := v1beta1.PatchTypeJSONPatch
		reviewResponse.PatchType = &pt
	}

	return &reviewResponse
}

type admitFunc func(v1beta1.AdmissionReview) *v1beta1.AdmissionResponse

func toAdmissionResponse(err error) *v1beta1.AdmissionResponse {
	return &v1beta1.AdmissionResponse{
		Result: &metav1.Status{
			Message: err.Error(),
		},
	}
}

func serve(w http.ResponseWriter, r *http.Request, admit admitFunc) {
	var body []byte
	var reviewResponse *v1beta1.AdmissionResponse
	var reqUID types.UID

	glog.V(2).Info("serve called.")

	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}

	if len(body) == 0 {
		glog.Error("No body in request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		glog.Errorf("contentType=%s, expect application/json", contentType)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// glog.V(2).Info(fmt.Sprintf("handling request: %v", body))
	ar := v1beta1.AdmissionReview{}
	deserializer := codecs.UniversalDeserializer()
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		glog.Error(err)
		reviewResponse = toAdmissionResponse(err)
	} else {
		if ar.Request == nil {
			err = errors.New("Request is empty")
			reviewResponse = toAdmissionResponse(err)
		} else {
			reqUID = ar.Request.UID
			reviewResponse = admit(ar)
		}
	}
	// glog.V(2).Info(fmt.Sprintf("sending response: %v", reviewResponse))

	response := v1beta1.AdmissionReview{}
	if reviewResponse != nil {
		response.Response = reviewResponse
		response.Response.UID = reqUID
	}

	// reset the Object and OldObject, they are not needed in a response.
	if ar.Request != nil {
		ar.Request.Object = runtime.RawExtension{}
		ar.Request.OldObject = runtime.RawExtension{}
	}

	resp, err := json.Marshal(response)
	if err != nil {
		glog.Error(err)
	}
	if _, err := w.Write(resp); err != nil {
		glog.Error(err)
	}
}

func servePods(w http.ResponseWriter, r *http.Request) {
	serve(w, r, mutatePods)
}

func main() {
	var certFile string
	var keyFile string

	flag.StringVar(&certFile, "tls-cert-file", certFile,
		"File containing the x509 Certificate for HTTPS. (CA cert, if any, concatenated after server cert).")
	flag.StringVar(&keyFile, "tls-private-key-file", keyFile, "File containing the x509 private key matching --tls-cert-file.")

	flag.Parse()

	if certFile == "" {
		glog.Error("TLS certificate file is not set")
		os.Exit(1)
	}

	if keyFile == "" {
		glog.Error("TLS private key is not set")
		os.Exit(1)
	}

	if _, err := os.Stat(certFile); err != nil {
		glog.Error("TLS certificate not found")
		os.Exit(1)
	}

	if _, err := os.Stat(keyFile); err != nil {
		glog.Error("TLS private key not found")
		os.Exit(1)
	}

	http.HandleFunc("/pods", servePods)

	glog.V(2).Info("Webhook started")

	server := &http.Server{
		Addr:      ":443",
		TLSConfig: getTLSConfig(certFile, keyFile),
	}

	glog.Fatal(server.ListenAndServeTLS("", ""))
}
