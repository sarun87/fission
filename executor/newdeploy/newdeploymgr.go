/*
Copyright 2016 The Fission Authors.

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

package newdeploy

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/fission/fission"
	"github.com/fission/fission/crd"
	"github.com/fission/fission/executor/fscache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api"
	apiv1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
	k8sCache "k8s.io/client-go/tools/cache"
)

type (
	requestType int

	NewDeploy struct {
		kubernetesClient *kubernetes.Clientset
		fissionClient    *crd.FissionClient
		crdClient        *rest.RESTClient
		instanceID       string

		fetcherImg             string
		fetcherImagePullPolicy apiv1.PullPolicy
		namespace              string
		sharedMountPath        string
		sharedSecretPath       string
		sharedCfgMapPath       string

		fsCache        *fscache.FunctionServiceCache // cache funcSvc's by function, address and podname
		requestChannel chan *fnRequest

		functions      []crd.Function
		funcStore      k8sCache.Store
		funcController k8sCache.Controller
	}

	fnRequest struct {
		reqType         requestType
		fn              *crd.Function
		responseChannel chan *fnResponse
	}

	fnResponse struct {
		error
		fSvc *fscache.FuncSvc
	}
)

const (
	FnCreate requestType = iota
	FnDelete
	FnUpdate
)

func MakeNewDeploy(
	fissionClient *crd.FissionClient,
	kubernetesClient *kubernetes.Clientset,
	crdClient *rest.RESTClient,
	namespace string,
	fsCache *fscache.FunctionServiceCache,
	instanceID string,
) *NewDeploy {

	log.Printf("Creating NewDeploy ExecutorType")

	fetcherImg := os.Getenv("FETCHER_IMAGE")
	if len(fetcherImg) == 0 {
		fetcherImg = "fission/fetcher"
	}
	fetcherImagePullPolicy := os.Getenv("FETCHER_IMAGE_PULL_POLICY")
	if len(fetcherImagePullPolicy) == 0 {
		fetcherImagePullPolicy = "IfNotPresent"
	}

	nd := &NewDeploy{
		fissionClient:    fissionClient,
		kubernetesClient: kubernetesClient,
		crdClient:        crdClient,
		instanceID:       instanceID,

		namespace: namespace,
		fsCache:   fsCache,

		fetcherImg:             fetcherImg,
		fetcherImagePullPolicy: apiv1.PullIfNotPresent,
		sharedMountPath:        "/userfunc",
		sharedSecretPath:       "/secrets",
		sharedCfgMapPath:       "/configs",

		requestChannel: make(chan *fnRequest),
	}

	if nd.crdClient != nil {
		fnStore, fnController := nd.initFuncController()
		nd.funcStore = fnStore
		nd.funcController = fnController
	}
	go nd.service()
	return nd
}

func (deploy *NewDeploy) Run(ctx context.Context) {
	go deploy.funcController.Run(ctx.Done())
}

func (deploy *NewDeploy) initFuncController() (k8sCache.Store, k8sCache.Controller) {
	resyncPeriod := 30 * time.Second
	listWatch := k8sCache.NewListWatchFromClient(deploy.crdClient, "functions", metav1.NamespaceDefault, fields.Everything())
	store, controller := k8sCache.NewInformer(listWatch, &crd.Function{}, resyncPeriod, k8sCache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			fn := obj.(*crd.Function)
			deploy.createFunction(fn)
		},
		DeleteFunc: func(obj interface{}) {
			fn := obj.(*crd.Function)
			deploy.deleteFunction(fn)
		},
		UpdateFunc: func(newObj interface{}, oldObj interface{}) {
			//TBD
		},
	})
	return store, controller
}

func (deploy *NewDeploy) service() {
	for {
		req := <-deploy.requestChannel
		switch req.reqType {
		case FnCreate:
			fsvc, err := deploy.fnCreate(req.fn)
			req.responseChannel <- &fnResponse{
				error: err,
				fSvc:  fsvc,
			}
			continue
		case FnUpdate:
			// TBD
		case FnDelete:
			_, err := deploy.fnDelete(req.fn)
			req.responseChannel <- &fnResponse{
				error: err,
				fSvc:  nil,
			}
			continue
		}
	}
}

func (deploy *NewDeploy) GetFuncSvc(metadata *metav1.ObjectMeta) (*fscache.FuncSvc, error) {
	c := make(chan *fnResponse)
	fn, err := deploy.fissionClient.Functions(metadata.Namespace).Get(metadata.Name)
	if err != nil {
		return nil, err
	}
	deploy.requestChannel <- &fnRequest{
		fn:              fn,
		reqType:         FnCreate,
		responseChannel: c,
	}
	resp := <-c
	if resp.error != nil {
		return nil, resp.error
	}
	return resp.fSvc, nil
}

func (deploy *NewDeploy) createFunction(fn *crd.Function) {
	if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fission.ExecutorTypeNewdeploy {
		return
	}
	if fn.Spec.InvokeStrategy.ExecutionStrategy.MinScale <= 0 {
		return
	}
	// Eager creation of function if minScale is greater than 0
	log.Printf("Eagerly creating newDeploy objects for function")
	c := make(chan *fnResponse)
	deploy.requestChannel <- &fnRequest{
		fn:              fn,
		reqType:         FnCreate,
		responseChannel: c,
	}
	resp := <-c
	if resp.error != nil {
		log.Printf("Error eager creating function: %v", resp.error)
	}
}

func (deploy *NewDeploy) deleteFunction(fn *crd.Function) {
	if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType == fission.ExecutorTypeNewdeploy {
		c := make(chan *fnResponse)
		deploy.requestChannel <- &fnRequest{
			fn:              fn,
			reqType:         FnDelete,
			responseChannel: c,
		}
		resp := <-c
		if resp.error != nil {
			log.Printf("Error deleing the function: %v", resp.error)
		}
	}
}

func (deploy *NewDeploy) fnCreate(fn *crd.Function) (*fscache.FuncSvc, error) {
	fsvc, err := deploy.fsCache.GetByFunction(&fn.Metadata)
	if err == nil {
		return fsvc, err
	}

	env, err := deploy.fissionClient.
		Environments(fn.Spec.Environment.Namespace).
		Get(fn.Spec.Environment.Name)
	if err != nil {
		return fsvc, err
	}

	objName := deploy.getObjName(fn)

	deployLabels := map[string]string{
		"environmentName":                 env.Metadata.Name,
		"environmentUid":                  string(env.Metadata.UID),
		"functionName":                    fn.Metadata.Name,
		"functionUid":                     string(fn.Metadata.UID),
		fission.EXECUTOR_INSTANCEID_LABEL: deploy.instanceID,
		"executorType":                    fission.ExecutorTypeNewdeploy,
	}

	depl, err := deploy.createOrGetDeployment(fn, env, objName, deployLabels)
	if err != nil {
		log.Printf("Error creating the deployment %v: %v", objName, err)
		return fsvc, err
	}

	svc, err := deploy.createOrGetSvc(deployLabels, objName)
	if err != nil {
		log.Printf("Error creating the service %v: %v", objName, err)
		return fsvc, err
	}
	svcAddress := svc.Spec.ClusterIP

	hpa, err := deploy.createOrGetHpa(objName, &fn.Spec.InvokeStrategy.ExecutionStrategy, depl)
	if err != nil {
		log.Printf("Error creating the HPA %v: %v", objName, err)
		return fsvc, err
	}

	kubeObjRefs := []api.ObjectReference{
		{
			//obj.TypeMeta.Kind does not work hence this, needs investigationa and a fix
			Kind:            "deployment",
			Name:            depl.ObjectMeta.Name,
			APIVersion:      depl.TypeMeta.APIVersion,
			Namespace:       depl.ObjectMeta.Namespace,
			ResourceVersion: depl.ObjectMeta.ResourceVersion,
			UID:             depl.ObjectMeta.UID,
		},
		{
			Kind:            "service",
			Name:            svc.ObjectMeta.Name,
			APIVersion:      svc.TypeMeta.APIVersion,
			Namespace:       svc.ObjectMeta.Namespace,
			ResourceVersion: svc.ObjectMeta.ResourceVersion,
			UID:             svc.ObjectMeta.UID,
		},
		{
			Kind:            "horizontalpodautoscaler",
			Name:            hpa.ObjectMeta.Name,
			APIVersion:      hpa.TypeMeta.APIVersion,
			Namespace:       hpa.ObjectMeta.Namespace,
			ResourceVersion: hpa.ObjectMeta.ResourceVersion,
			UID:             hpa.ObjectMeta.UID,
		},
	}

	fsvc = &fscache.FuncSvc{
		Name:              objName,
		Function:          &fn.Metadata,
		Environment:       env,
		Address:           svcAddress,
		KubernetesObjects: kubeObjRefs,
		Executor:          fscache.NEWDEPLOY,
	}

	_, err = deploy.fsCache.Add(*fsvc)
	if err != nil {
		log.Printf("Error adding the function to cache: %v", err)
		return fsvc, err
	}
	return fsvc, nil
}

func (deploy *NewDeploy) fnDelete(fn *crd.Function) (*fscache.FuncSvc, error) {

	var delError error

	fsvc, err := deploy.fsCache.GetByFunction(&fn.Metadata)
	if err != nil {
		log.Printf("fsvc not fonud in cache: %v", fn.Metadata)
		delError = err
	} else {
		_, err = deploy.fsCache.DeleteOld(fsvc, time.Second*0)
		if err != nil {
			log.Printf("Error deleting the function from cache: %v", fsvc)
			delError = err
		}
	}
	objName := fsvc.Name

	err = deploy.deleteDeployment(deploy.namespace, objName)
	if err != nil {
		log.Printf("Error deleting the deployment: %v", objName)
		delError = err
	}

	err = deploy.deleteSvc(deploy.namespace, objName)
	if err != nil {
		log.Printf("Error deleting the service: %v", objName)
		delError = err
	}

	err = deploy.deleteHpa(deploy.namespace, objName)
	if err != nil {
		log.Printf("Error deleting the HPA: %v", objName)
		delError = err
	}

	if delError != nil {
		return nil, delError
	}
	return nil, nil
}

func (deploy *NewDeploy) getObjName(fn *crd.Function) string {
	return fmt.Sprintf("%v-%v",
		fn.Metadata.Name,
		deploy.instanceID)
}
