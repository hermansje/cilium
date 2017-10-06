// Copyright 2016-2017 Authors of Cilium
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
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/cilium/cilium/common/types"
	"github.com/cilium/cilium/pkg/k8s"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logfields"
	"github.com/cilium/cilium/pkg/node"
	"github.com/cilium/cilium/pkg/nodeaddress"

	log "github.com/sirupsen/logrus"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	networkingv1 "k8s.io/client-go/pkg/apis/networking/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	k8sErrLogTimeout = time.Minute
)

var (
	k8sErrMsgMU lock.RWMutex
	// k8sErrMsg stores a timer for each k8s error message received
	k8sErrMsg                    = map[string]*time.Timer{}
	stopPolicyController         = make(chan struct{})
	restartCiliumRulesController = make(chan struct{})

	// cnpClient is the interface for CRD and TPR
	cnpClient k8s.CNPCliInterface

	// ciliumRulesStore is the local cache for the CNP
	ciliumRulesStore cache.Store
)

func init() {
	// Replace error handler with our own
	runtime.ErrorHandlers = []func(error){
		k8sErrorHandler,
	}
}

// k8sErrorHandler handles the error messages on a non verbose way by omitting
// same error messages for a timeout defined with k8sErrLogTimeout.
func k8sErrorHandler(e error) {
	if e == nil {
		return
	}
	errstr := e.Error()
	k8sErrMsgMU.Lock()
	// if the error message already exists in the map then in print it
	// otherwise we create a new timer for that specific message in the
	// k8sErrMsg map.
	if t, ok := k8sErrMsg[errstr]; !ok {
		// Omitting the 'connection refused' common messages
		if strings.Contains(errstr, "connection refused") {
			k8sErrMsg[errstr] = time.NewTimer(k8sErrLogTimeout)
			k8sErrMsgMU.Unlock()
		} else {
			if strings.Contains(errstr, "Failed to list *v1.NetworkPolicy: the server could not find the requested resource") {
				k8sErrMsgMU.Unlock()
				log.Warn("Consider upgrading kubernetes to >=1.7 to enforce NetworkPolicy version 1")
				stopPolicyController <- struct{}{}
			} else if strings.Contains(errstr, "Failed to list *k8s.CiliumNetworkPolicy: the server could not find the requested resource") {
				k8sErrMsg[errstr] = time.NewTimer(k8sErrLogTimeout)
				k8sErrMsgMU.Unlock()
				log.Warn("Detected conflicting TPR and CRD, please migrate all ThirdPartyResource to CustomResourceDefinition! More info: https://cilium.link/migrate-tpr")
				log.Warn("Due to conflicting TPR and CRD rules, CiliumNetworkPolicy enforcement can't be guaranteed!")
			} else if strings.Contains(errstr, "Unable to decode an event from the watch stream: unable to decode watch event") || strings.Contains(errstr, "Failed to list *k8s.CiliumNetworkPolicy: only encoded map or array can be decoded into a struct") {
				k8sErrMsg[errstr] = time.NewTimer(k8sErrLogTimeout)
				k8sErrMsgMU.Unlock()
				log.Warn("Unable to decode an event from watch, restarting cilium policy rules controller")
				restartCiliumRulesController <- struct{}{}
			}
		}
	} else {
		k8sErrMsgMU.Unlock()
		select {
		case <-t.C:
			log.WithError(e).Error("k8sError")
			t.Reset(k8sErrLogTimeout)
		default:
		}
		return
	}
	// Still log other error messages
	log.WithError(e).Error("k8sError")
}

// EnableK8sWatcher watches for policy, services and endpoint changes on the Kubernetes
// api server defined in the receiver's daemon k8sClient. Re-syncs all state from the
// Kubernetes api server at the given reSyncPeriod duration.
func (d *Daemon) EnableK8sWatcher(reSyncPeriod time.Duration) error {
	if !k8s.IsEnabled() {
		return nil
	}

	restConfig, err := k8s.CreateConfig()
	if err != nil {
		return fmt.Errorf("Unable to create rest configuration: %s", err)
	}

	apiextensionsclientset, err := apiextensionsclient.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("Unable to create rest configuration for k8s CRD: %s", err)
	}

	if err := k8s.CreateCustomResourceDefinitions(apiextensionsclientset); errors.IsNotFound(err) {
		// If CRD was not found it means we are running in k8s <1.7
		// then we should set up TPR instead
		log.Debug("Detected k8s <1.7, using TPR instead of CRD")
		if err := k8s.CreateThirdPartyResourcesDefinitions(k8s.Client()); err != nil {
			return fmt.Errorf("Unable to create third party resource: %s", err)
		}
		cnpClient, err = k8s.CreateTPRClient(restConfig)
		if err != nil {
			return fmt.Errorf("Unable to create third party resource client: %s", err)
		}
	} else if err != nil {
		return fmt.Errorf("Unable to create custom resource definition: %s", err)
	} else {
		cnpClient, err = k8s.CreateCRDClient(restConfig)
		if err != nil {
			return fmt.Errorf("Unable to create custom resource definition client: %s", err)
		}
	}

	_, policyControllerDeprecated := cache.NewInformer(
		cache.NewListWatchFromClient(k8s.Client().ExtensionsV1beta1().RESTClient(),
			"networkpolicies", v1.NamespaceAll, fields.Everything()),
		&v1beta1.NetworkPolicy{},
		reSyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    d.addK8sNetworkPolicyDeprecated,
			UpdateFunc: d.updateK8sNetworkPolicyDeprecated,
			DeleteFunc: d.deleteK8sNetworkPolicyDeprecated,
		},
	)
	go policyControllerDeprecated.Run(wait.NeverStop)

	_, policyController := cache.NewInformer(
		cache.NewListWatchFromClient(k8s.Client().NetworkingV1().RESTClient(),
			"networkpolicies", v1.NamespaceAll, fields.Everything()),
		&networkingv1.NetworkPolicy{},
		reSyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    d.addK8sNetworkPolicy,
			UpdateFunc: d.updateK8sNetworkPolicy,
			DeleteFunc: d.deleteK8sNetworkPolicy,
		},
	)
	go policyController.Run(stopPolicyController)

	_, svcController := cache.NewInformer(
		cache.NewListWatchFromClient(k8s.Client().CoreV1().RESTClient(),
			"services", v1.NamespaceAll, fields.Everything()),
		&v1.Service{},
		reSyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    d.serviceAddFn,
			UpdateFunc: d.serviceModFn,
			DeleteFunc: d.serviceDelFn,
		},
	)
	go svcController.Run(wait.NeverStop)

	_, endpointController := cache.NewInformer(
		cache.NewListWatchFromClient(k8s.Client().CoreV1().RESTClient(),
			"endpoints", v1.NamespaceAll, fields.Everything()),
		&v1.Endpoints{},
		reSyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    d.endpointAddFn,
			UpdateFunc: d.endpointModFn,
			DeleteFunc: d.endpointDelFn,
		},
	)
	go endpointController.Run(wait.NeverStop)

	_, ingressController := cache.NewInformer(
		cache.NewListWatchFromClient(k8s.Client().ExtensionsV1beta1().RESTClient(),
			"ingresses", v1.NamespaceAll, fields.Everything()),
		&v1beta1.Ingress{},
		reSyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    d.ingressAddFn,
			UpdateFunc: d.ingressModFn,
			DeleteFunc: d.ingressDelFn,
		},
	)
	go ingressController.Run(wait.NeverStop)

	ciliumNetworkPolicyHandler := cache.ResourceEventHandlerFuncs{
		AddFunc:    d.addCiliumNetworkPolicy,
		UpdateFunc: d.updateCiliumNetworkPolicy,
		DeleteFunc: d.deleteCiliumNetworkPolicy,
	}

	var ciliumRulesController cache.Controller
	ciliumRulesStore, ciliumRulesController = cache.NewInformer(
		cnpClient.NewListWatch(),
		&k8s.CiliumNetworkPolicy{},
		reSyncPeriod,
		ciliumNetworkPolicyHandler,
	)

	stopCiliumRulesController := make(chan struct{})
	go ciliumRulesController.Run(stopCiliumRulesController)

	go func() {
		for range restartCiliumRulesController {
			log.Debug("Received Cilium Rules Controller restart signal")
			// We need to send stop signal to channel and close it for controller queue to close
			stopCiliumRulesController <- struct{}{}
			close(stopCiliumRulesController)
			// we need to create new controller after stopping old one
			ciliumRulesStore, ciliumRulesController = cache.NewInformer(
				cnpClient.NewListWatch(),
				&k8s.CiliumNetworkPolicy{},
				reSyncPeriod,
				ciliumNetworkPolicyHandler,
			)
			stopCiliumRulesController = make(chan struct{})
			go ciliumRulesController.Run(stopCiliumRulesController)
		}
	}()

	_, nodesController := cache.NewInformer(
		cache.NewListWatchFromClient(k8s.Client().CoreV1().RESTClient(),
			"nodes", v1.NamespaceAll, fields.Everything()),
		&v1.Node{},
		reSyncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    d.nodesAddFn,
			UpdateFunc: d.nodesModFn,
			DeleteFunc: d.nodesDelFn,
		},
	)
	go nodesController.Run(wait.NeverStop)
	return nil
}

func (d *Daemon) addK8sNetworkPolicy(obj interface{}) {
	k8sNP, ok := obj.(*networkingv1.NetworkPolicy)
	if !ok {
		log.Error("Ignoring invalid k8s NetworkPolicy addition")
		return
	}
	rules, err := k8s.ParseNetworkPolicy(k8sNP)
	if err != nil {
		log.WithError(err).WithField(logfields.Object, logfields.Repr(obj)).Error("Error while parsing kubernetes network policy")
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sNetworkPolicyName: k8sNP.Name,
		logfields.K8sNetworkPolicy:     logfields.Repr(k8sNP),
	})

	opts := AddOptions{Replace: true}
	if _, err := d.PolicyAdd(rules, &opts); err != nil {
		scopedLog.WithError(err).WithField(logfields.Object, logfields.Repr(rules)).Error("Error while adding kubernetes network policy")
		return
	}

	scopedLog.Info("Kubernetes network policy successfully added")
}

func (d *Daemon) updateK8sNetworkPolicy(oldObj interface{}, newObj interface{}) {
	log.WithFields(log.Fields{
		"obj.old": logfields.Repr(oldObj),
		"obj.new": logfields.Repr(newObj),
	}).Debug("Modified policy")

	d.addK8sNetworkPolicy(newObj)
}

func (d *Daemon) deleteK8sNetworkPolicy(obj interface{}) {
	k8sNP, ok := obj.(*networkingv1.NetworkPolicy)
	if !ok {
		log.Error("Ignoring invalid k8s NetworkPolicy deletion")
		return
	}

	labels := labels.ParseSelectLabelArray(k8s.ExtractPolicyName(k8sNP))

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sNetworkPolicyName: k8sNP.Name,
		logfields.Labels:               logfields.Repr(labels),
	})
	if _, err := d.PolicyDelete(labels); err != nil {
		scopedLog.WithError(err).Error("Error while deleting kubernetes network policy")
	} else {
		scopedLog.Info("Kubernetes network policy successfully removed")
	}
}

// addK8sNetworkPolicyDeprecated FIXME remove in k8s 1.8
func (d *Daemon) addK8sNetworkPolicyDeprecated(obj interface{}) {
	k8sNP, ok := obj.(*v1beta1.NetworkPolicy)
	if !ok {
		log.Error("Ignoring invalid k8s v1beta1 NetworkPolicy addition")
		return
	}
	rules, err := k8s.ParseNetworkPolicyDeprecated(k8sNP)
	if err != nil {
		log.WithError(err).WithField(logfields.Object, logfields.Repr(obj)).Error("Error while parsing kubernetes v1beta1 network policy")
		return
	}

	scopedLog := log.WithField(logfields.K8sNetworkPolicyName, k8sNP.Name)

	opts := AddOptions{Replace: true}
	if _, err := d.PolicyAdd(rules, &opts); err != nil {
		scopedLog.WithField(logfields.Object, logfields.Repr(rules)).Error("Error while parsing kubernetes v1beta1 network policy")
		return
	}

	scopedLog.Info("Kubernetes v1beta1 network policy successfully added")
}

// updateK8sNetworkPolicyDeprecated FIXME remove in k8s 1.8
func (d *Daemon) updateK8sNetworkPolicyDeprecated(oldObj interface{}, newObj interface{}) {
	log.WithFields(log.Fields{
		"obj.old": oldObj,
		"obj.new": newObj,
	}).Debug("Modified v1beta1 policy")

	d.addK8sNetworkPolicyDeprecated(newObj)
}

// deleteK8sNetworkPolicyDeprecated FIXME remove in k8s 1.8
func (d *Daemon) deleteK8sNetworkPolicyDeprecated(obj interface{}) {
	k8sNP, ok := obj.(*v1beta1.NetworkPolicy)
	if !ok {
		log.Error("Ignoring invalid k8s v1beta1.NetworkPolicy deletion")
		return
	}

	labels := labels.ParseSelectLabelArray(k8s.ExtractPolicyNameDeprecated(k8sNP))

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sNetworkPolicyName: k8sNP.Name,
		logfields.Labels:               logfields.Repr(labels),
	})

	if _, err := d.PolicyDelete(labels); err != nil {
		scopedLog.WithError(err).Error("Error while deleting v1beta1 kubernetes network policy")
		return
	}

	scopedLog.Info("Kubernetes v1beta1 network policy successfully removed")
}

func (d *Daemon) serviceAddFn(obj interface{}) {
	svc, ok := obj.(*v1.Service)
	if !ok {
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sSvcName:   svc.Name,
		logfields.K8sNamespace: svc.Namespace,
		logfields.K8sSvcType:   svc.Spec.Type,
	})

	switch svc.Spec.Type {
	case v1.ServiceTypeClusterIP, v1.ServiceTypeNodePort, v1.ServiceTypeLoadBalancer:
		break

	case v1.ServiceTypeExternalName:
		// External-name services must be ignored
		return

	default:
		scopedLog.Warn("Ignoring k8s service: unsupported type")
		return
	}

	if strings.ToLower(svc.Spec.ClusterIP) == "none" || svc.Spec.ClusterIP == "" {
		scopedLog.Info("Ignoring k8s service: headless")
		return
	}

	svcns := types.K8sServiceNamespace{
		Service:   svc.Name,
		Namespace: svc.Namespace,
	}

	clusterIP := net.ParseIP(svc.Spec.ClusterIP)
	newSI := types.NewK8sServiceInfo(clusterIP)

	// FIXME: Add support for
	//  - NodePort

	for _, port := range svc.Spec.Ports {
		p, err := types.NewFEPort(types.L4Type(port.Protocol), uint16(port.Port))
		if err != nil {
			scopedLog.WithError(err).WithField("port", port).Error("Unable to add service port")
			continue
		}
		if _, ok := newSI.Ports[types.FEPortName(port.Name)]; !ok {
			newSI.Ports[types.FEPortName(port.Name)] = p
		}
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()

	d.loadBalancer.K8sServices[svcns] = newSI

	d.syncLB(&svcns, nil, nil)
}

func (d *Daemon) serviceModFn(_ interface{}, newObj interface{}) {
	newSvc, ok := newObj.(*v1.Service)
	if !ok {
		return
	}
	log.WithField(logfields.Object, logfields.Repr(newSvc)).Debug("Service ModFn")

	d.serviceAddFn(newObj)
}

func (d *Daemon) serviceDelFn(obj interface{}) {
	svc, ok := obj.(*v1.Service)
	if !ok {
		return
	}
	log.WithFields(log.Fields{
		logfields.K8sSvcName:   svc.Name,
		logfields.K8sNamespace: svc.Namespace,
	}).Debug("Deleting k8s service")

	svcns := &types.K8sServiceNamespace{
		Service:   svc.Name,
		Namespace: svc.Namespace,
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()
	d.syncLB(nil, nil, svcns)
}

func (d *Daemon) endpointAddFn(obj interface{}) {
	ep, ok := obj.(*v1.Endpoints)
	if !ok {
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sEndpointName: ep.Name,
		logfields.K8sNamespace:    ep.Namespace,
	})

	svcns := types.K8sServiceNamespace{
		Service:   ep.Name,
		Namespace: ep.Namespace,
	}

	newSvcEP := types.NewK8sServiceEndpoint()

	for _, sub := range ep.Subsets {
		for _, addr := range sub.Addresses {
			newSvcEP.BEIPs[addr.IP] = true
		}
		for _, port := range sub.Ports {
			lbPort, err := types.NewL4Addr(types.L4Type(port.Protocol), uint16(port.Port))
			if err != nil {
				scopedLog.WithError(err).Error("Error while creating a new LB Port")
				continue
			}
			newSvcEP.Ports[types.FEPortName(port.Name)] = lbPort
		}
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()

	d.loadBalancer.K8sEndpoints[svcns] = newSvcEP

	d.syncLB(&svcns, nil, nil)

	if d.conf.IsLBEnabled() {
		if err := d.syncExternalLB(&svcns, nil, nil); err != nil {
			scopedLog.WithError(err).Error("Unable to add endpoints on ingress service")
			return
		}
	}
}

func (d *Daemon) endpointModFn(_ interface{}, newObj interface{}) {
	_, ok := newObj.(*v1.Endpoints)
	if !ok {
		return
	}

	d.endpointAddFn(newObj)
}

func (d *Daemon) endpointDelFn(obj interface{}) {
	ep, ok := obj.(*v1.Endpoints)
	if !ok {
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sEndpointName: ep.Name,
		logfields.K8sNamespace:    ep.Namespace,
	})

	svcns := &types.K8sServiceNamespace{
		Service:   ep.Name,
		Namespace: ep.Namespace,
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()

	d.syncLB(nil, nil, svcns)
	if d.conf.IsLBEnabled() {
		if err := d.syncExternalLB(nil, nil, svcns); err != nil {
			scopedLog.WithError(err).Error("Unable to remove endpoints on ingress service")
			return
		}
	}
}

func areIPsConsistent(ipv4Enabled, isSvcIPv4 bool, svc types.K8sServiceNamespace, se *types.K8sServiceEndpoint) error {
	if isSvcIPv4 {
		if !ipv4Enabled {
			return fmt.Errorf("Received an IPv4 kubernetes service but IPv4 is "+
				"disabled in the cilium daemon. Ignoring service %+v", svc)
		}

		for epIP := range se.BEIPs {
			//is IPv6?
			if net.ParseIP(epIP).To4() == nil {
				return fmt.Errorf("Not all endpoints IPs are IPv4. Ignoring IPv4 service %+v", svc)
			}
		}
	} else {
		for epIP := range se.BEIPs {
			//is IPv4?
			if net.ParseIP(epIP).To4() != nil {
				return fmt.Errorf("Not all endpoints IPs are IPv6. Ignoring IPv6 service %+v", svc)
			}
		}
	}
	return nil
}

func getUniqPorts(svcPorts map[types.FEPortName]*types.FEPort) map[uint16]bool {
	// We are not discriminating the different L4 protocols on the same L4
	// port so we create the number of unique sets of service IP + service
	// port.
	uniqPorts := map[uint16]bool{}
	for _, svcPort := range svcPorts {
		uniqPorts[svcPort.Port] = true
	}
	return uniqPorts
}

func (d *Daemon) delK8sSVCs(svc types.K8sServiceNamespace, svcInfo *types.K8sServiceInfo, se *types.K8sServiceEndpoint) error {
	isSvcIPv4 := svcInfo.FEIP.To4() != nil
	if err := areIPsConsistent(!d.conf.IPv4Disabled, isSvcIPv4, svc, se); err != nil {
		return err
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sSvcName:   svc.Service,
		logfields.K8sNamespace: svc.Namespace,
	})

	repPorts := getUniqPorts(svcInfo.Ports)

	for _, svcPort := range svcInfo.Ports {
		if !repPorts[svcPort.Port] {
			continue
		}
		repPorts[svcPort.Port] = false

		if svcPort.ID != 0 {
			if err := DeleteL3n4AddrIDByUUID(uint32(svcPort.ID)); err != nil {
				scopedLog.WithError(err).Warn("Error while cleaning service ID")
			}
		}

		fe, err := types.NewL3n4Addr(svcPort.Protocol, svcInfo.FEIP, svcPort.Port)
		if err != nil {
			scopedLog.WithError(err).Error("Error while creating a New L3n4AddrID. Ignoring service")
			continue
		}

		if err := d.svcDeleteByFrontend(fe); err != nil {
			scopedLog.WithError(err).WithField(logfields.Object, logfields.Repr(fe)).Warn("Error deleting service by frontend")

		} else {
			scopedLog.Debugf("# cilium lb delete-service %s %d 0", svcInfo.FEIP, svcPort.Port)
		}

		if err := d.RevNATDelete(svcPort.ID); err != nil {
			scopedLog.WithError(err).WithField(logfields.ServiceID, svcPort.ID).Warn("Error deleting reverse NAT")
		} else {
			scopedLog.Debugf("# cilium lb delete-rev-nat %d", svcPort.ID)
		}
	}
	return nil
}

func (d *Daemon) addK8sSVCs(svc types.K8sServiceNamespace, svcInfo *types.K8sServiceInfo, se *types.K8sServiceEndpoint) error {
	scopedLog := log.WithFields(log.Fields{
		logfields.K8sSvcName:   svc.Service,
		logfields.K8sNamespace: svc.Namespace,
	})

	isSvcIPv4 := svcInfo.FEIP.To4() != nil
	if err := areIPsConsistent(!d.conf.IPv4Disabled, isSvcIPv4, svc, se); err != nil {
		return err
	}

	uniqPorts := getUniqPorts(svcInfo.Ports)

	for fePortName, fePort := range svcInfo.Ports {
		if !uniqPorts[fePort.Port] {
			continue
		}

		k8sBEPort := se.Ports[fePortName]
		uniqPorts[fePort.Port] = false

		if fePort.ID == 0 {
			feAddr, err := types.NewL3n4Addr(fePort.Protocol, svcInfo.FEIP, fePort.Port)
			if err != nil {
				scopedLog.WithError(err).WithFields(log.Fields{
					logfields.ServiceID: fePortName,
					logfields.IPAddr:    svcInfo.FEIP,
					logfields.Port:      fePort.Port,
					logfields.Protocol:  fePort.Protocol,
				}).Error("Error while creating a new L3n4Addr. Ignoring service...")
				continue
			}
			feAddrID, err := PutL3n4Addr(*feAddr, 0)
			if err != nil {
				scopedLog.WithError(err).WithFields(log.Fields{
					logfields.ServiceID: fePortName,
					logfields.IPAddr:    svcInfo.FEIP,
					logfields.Port:      fePort.Port,
					logfields.Protocol:  fePort.Protocol,
				}).Error("Error while getting a new service ID. Ignoring service...")
				continue
			}
			scopedLog.WithFields(log.Fields{
				logfields.ServiceName: fePortName,
				logfields.ServiceID:   feAddrID.ID,
				logfields.Object:      logfields.Repr(svc),
			}).Debug("Got feAddr ID for service")
			fePort.ID = feAddrID.ID
		}

		besValues := []types.LBBackEnd{}

		if k8sBEPort != nil {
			for epIP := range se.BEIPs {
				bePort := types.LBBackEnd{
					L3n4Addr: types.L3n4Addr{IP: net.ParseIP(epIP), L4Addr: *k8sBEPort},
					Weight:   0,
				}
				besValues = append(besValues, bePort)
			}
		}

		fe, err := types.NewL3n4AddrID(fePort.Protocol, svcInfo.FEIP, fePort.Port, fePort.ID)
		if err != nil {
			scopedLog.WithError(err).WithFields(log.Fields{
				logfields.IPAddr: svcInfo.FEIP,
				logfields.Port:   svcInfo.Ports,
			}).Error("Error while creating a New L3n4AddrID. Ignoring service...")
			continue
		}
		if _, err := d.svcAdd(*fe, besValues, true); err != nil {
			scopedLog.WithError(err).Error("Error while inserting service in LB map")
		}
	}
	return nil
}

func (d *Daemon) syncLB(newSN, modSN, delSN *types.K8sServiceNamespace) {
	deleteSN := func(delSN types.K8sServiceNamespace) {
		svc, ok := d.loadBalancer.K8sServices[delSN]
		if !ok {
			delete(d.loadBalancer.K8sEndpoints, delSN)
			return
		}

		endpoint, ok := d.loadBalancer.K8sEndpoints[delSN]
		if !ok {
			delete(d.loadBalancer.K8sServices, delSN)
			return
		}

		if err := d.delK8sSVCs(delSN, svc, endpoint); err != nil {
			log.WithError(err).WithFields(log.Fields{
				logfields.K8sSvcName:   delSN.Service,
				logfields.K8sNamespace: delSN.Namespace,
			}).Error("Unable to delete k8s service")
			return
		}

		delete(d.loadBalancer.K8sServices, delSN)
		delete(d.loadBalancer.K8sEndpoints, delSN)
	}

	addSN := func(addSN types.K8sServiceNamespace) {
		svcInfo, ok := d.loadBalancer.K8sServices[addSN]
		if !ok {
			return
		}

		endpoint, ok := d.loadBalancer.K8sEndpoints[addSN]
		if !ok {
			return
		}

		if err := d.addK8sSVCs(addSN, svcInfo, endpoint); err != nil {
			log.WithError(err).WithFields(log.Fields{
				logfields.K8sSvcName:   addSN.Service,
				logfields.K8sNamespace: addSN.Namespace,
			}).Error("Unable to add k8s service")
		}
	}

	if delSN != nil {
		// Clean old services
		deleteSN(*delSN)
	}
	if modSN != nil {
		// Re-add modified services
		addSN(*modSN)
	}
	if newSN != nil {
		// Add new services
		addSN(*newSN)
	}
}

func (d *Daemon) ingressAddFn(obj interface{}) {
	if !d.conf.IsLBEnabled() {
		// Add operations don't matter to non-LB nodes.
		return
	}
	ingress, ok := obj.(*v1beta1.Ingress)
	if !ok {
		return
	}

	if ingress.Spec.Backend == nil {
		// We only support Single Service Ingress for now
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sSvcName:   ingress.Spec.Backend.ServiceName,
		logfields.K8sNamespace: ingress.Namespace,
	})

	svcName := types.K8sServiceNamespace{
		Service:   ingress.Spec.Backend.ServiceName,
		Namespace: ingress.Namespace,
	}

	ingressPort := ingress.Spec.Backend.ServicePort.IntValue()
	fePort, err := types.NewFEPort(types.TCP, uint16(ingressPort))
	if err != nil {
		return
	}

	var host net.IP
	if d.conf.IPv4Disabled {
		host = d.conf.HostV6Addr
	} else {
		host = d.conf.HostV4Addr
	}
	ingressSvcInfo := types.NewK8sServiceInfo(host)
	ingressSvcInfo.Ports[types.FEPortName(ingress.Spec.Backend.ServicePort.StrVal)] = fePort

	syncIngress := func(ingressSvcInfo *types.K8sServiceInfo) error {
		d.loadBalancer.K8sIngress[svcName] = ingressSvcInfo

		if err := d.syncExternalLB(&svcName, nil, nil); err != nil {
			return fmt.Errorf("Unable to add ingress service %s: %s", svcName, err)
		}
		return nil
	}

	d.loadBalancer.K8sMU.Lock()
	err = syncIngress(ingressSvcInfo)
	d.loadBalancer.K8sMU.Unlock()
	if err != nil {
		scopedLog.WithError(err).Error("Error in syncIngress")
		return
	}

	hostname, _ := os.Hostname()
	ingress.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{
		{
			IP:       host.String(),
			Hostname: hostname,
		},
	}

	_, err = k8s.Client().Extensions().Ingresses(ingress.Namespace).UpdateStatus(ingress)
	if err != nil {
		scopedLog.WithError(err).WithFields(log.Fields{
			logfields.K8sIngress: ingress,
		}).Error("Unable to update status of ingress")
		return
	}
}

func (d *Daemon) ingressModFn(oldObj interface{}, newObj interface{}) {
	oldIngress, ok := oldObj.(*v1beta1.Ingress)
	if !ok {
		return
	}
	newIngress, ok := newObj.(*v1beta1.Ingress)
	if !ok {
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sNetworkPolicyName: newIngress.Name,
		logfields.K8sNamespace:         newIngress.Namespace,
	})

	if oldIngress.Spec.Backend == nil || newIngress.Spec.Backend == nil {
		// We only support Single Service Ingress for now
		return
	}

	// Add RevNAT to the BPF Map for non-LB nodes when a LB node update the
	// ingress status with its address.
	if !d.conf.IsLBEnabled() {
		port := newIngress.Spec.Backend.ServicePort.IntValue()
		for _, loadbalancer := range newIngress.Status.LoadBalancer.Ingress {
			ingressIP := net.ParseIP(loadbalancer.IP)
			if ingressIP == nil {
				continue
			}
			feAddr, err := types.NewL3n4Addr(types.TCP, ingressIP, uint16(port))
			if err != nil {
				scopedLog.WithError(err).Error("Error while creating a new L3n4Addr. Ignoring ingress...")
				continue
			}
			feAddrID, err := PutL3n4Addr(*feAddr, 0)
			if err != nil {
				scopedLog.WithError(err).Error("Error while getting a new service ID. Ignoring ingress...")
				continue
			}
			scopedLog.WithFields(log.Fields{
				logfields.ServiceID: feAddrID.ID,
			}).Debug("Got service ID for ingress")

			if err := d.RevNATAdd(feAddrID.ID, feAddrID.L3n4Addr); err != nil {
				scopedLog.WithError(err).WithFields(log.Fields{
					logfields.ServiceID: feAddrID.ID,
					logfields.IPAddr:    feAddrID.L3n4Addr.IP,
					logfields.Port:      feAddrID.L3n4Addr.Port,
					logfields.Protocol:  feAddrID.L3n4Addr.Protocol,
				}).Error("Unable to add reverse NAT ID for ingress")
			}
		}
		return
	}

	if oldIngress.Spec.Backend.ServiceName == newIngress.Spec.Backend.ServiceName &&
		oldIngress.Spec.Backend.ServicePort == newIngress.Spec.Backend.ServicePort {
		return
	}

	d.ingressAddFn(newObj)
}

func (d *Daemon) ingressDelFn(obj interface{}) {
	ing, ok := obj.(*v1beta1.Ingress)
	if !ok {
		return
	}

	if ing.Spec.Backend == nil {
		// We only support Single Service Ingress for now
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.K8sIngressName: ing.Name,
		logfields.K8sSvcName:     ing.Spec.Backend.ServiceName,
		logfields.K8sNamespace:   ing.Namespace,
	})

	svcName := types.K8sServiceNamespace{
		Service:   ing.Spec.Backend.ServiceName,
		Namespace: ing.Namespace,
	}

	// Remove RevNAT from the BPF Map for non-LB nodes.
	if !d.conf.IsLBEnabled() {
		port := ing.Spec.Backend.ServicePort.IntValue()
		for _, loadbalancer := range ing.Status.LoadBalancer.Ingress {
			ingressIP := net.ParseIP(loadbalancer.IP)
			if ingressIP == nil {
				continue
			}
			feAddr, err := types.NewL3n4Addr(types.TCP, ingressIP, uint16(port))
			if err != nil {
				scopedLog.WithError(err).Error("Error while creating a new L3n4Addr. Ignoring ingress...")
				continue
			}
			// This is the only way that we can get the service's ID
			// without accessing the KVStore.
			svc := d.svcGetBySHA256Sum(feAddr.SHA256Sum())
			if svc != nil {
				if err := d.RevNATDelete(svc.FE.ID); err != nil {
					scopedLog.WithError(err).WithFields(log.Fields{
						logfields.ServiceID: svc.FE.ID,
					}).Error("Error while removing RevNAT for ingress")
				}
			}
		}
		return
	}

	d.loadBalancer.K8sMU.Lock()
	defer d.loadBalancer.K8sMU.Unlock()

	ingressSvcInfo, ok := d.loadBalancer.K8sIngress[svcName]
	if !ok {
		return
	}

	// Get all active endpoints for the service specified in ingress
	k8sEP, ok := d.loadBalancer.K8sEndpoints[svcName]
	if !ok {
		return
	}

	err := d.delK8sSVCs(svcName, ingressSvcInfo, k8sEP)
	if err != nil {
		scopedLog.WithError(err).Error("Unable to delete K8s ingress")
		return
	}
	delete(d.loadBalancer.K8sIngress, svcName)
}

func (d *Daemon) syncExternalLB(newSN, modSN, delSN *types.K8sServiceNamespace) error {
	deleteSN := func(delSN types.K8sServiceNamespace) error {
		ingSvc, ok := d.loadBalancer.K8sIngress[delSN]
		if !ok {
			return nil
		}

		endpoint, ok := d.loadBalancer.K8sEndpoints[delSN]
		if !ok {
			return nil
		}

		if err := d.delK8sSVCs(delSN, ingSvc, endpoint); err != nil {
			return err
		}

		delete(d.loadBalancer.K8sServices, delSN)
		return nil
	}

	addSN := func(addSN types.K8sServiceNamespace) error {
		ingressSvcInfo, ok := d.loadBalancer.K8sIngress[addSN]
		if !ok {
			return nil
		}

		k8sEP, ok := d.loadBalancer.K8sEndpoints[addSN]
		if !ok {
			return nil
		}

		err := d.addK8sSVCs(addSN, ingressSvcInfo, k8sEP)
		if err != nil {
			return err
		}
		return nil
	}

	if delSN != nil {
		// Clean old services
		return deleteSN(*delSN)
	}
	if modSN != nil {
		// Re-add modified services
		return addSN(*modSN)
	}
	if newSN != nil {
		// Add new services
		return addSN(*newSN)
	}
	return nil
}

func (d *Daemon) addCiliumNetworkPolicy(obj interface{}) {
	rule, ok := obj.(*k8s.CiliumNetworkPolicy)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).Warn("Received unknown object, expected a CiliumNetworkPolicy object")
		return
	}

	log.WithField(logfields.CiliumNetworkPolicy, logfields.Repr(rule)).Debug("Adding CiliumNetworkPolicy")

	rules, err := rule.Parse()
	if err == nil {
		if len(rules) > 0 {
			_, err = d.PolicyAdd(rules, &AddOptions{Replace: true})
		}
	}

	var cnpns k8s.CiliumNetworkPolicyNodeStatus
	if err != nil {
		cnpns = k8s.CiliumNetworkPolicyNodeStatus{
			OK:          false,
			Error:       fmt.Sprintf("%s", err),
			LastUpdated: time.Now(),
		}
		log.WithError(err).WithFields(log.Fields{
			logfields.CiliumNetworkPolicyName: rule.Metadata.Name,
		}).Warnf("Unable to add CiliumNetworkPolicy. err != nil: '%t'. a nil object: '%s'", err != nil, nil)
	} else {
		cnpns = k8s.CiliumNetworkPolicyNodeStatus{
			OK:          true,
			LastUpdated: time.Now(),
		}
		log.WithError(err).WithFields(log.Fields{
			logfields.CiliumNetworkPolicyName: rule.Metadata.Name,
		}).Info("Imported CiliumNetworkPolicy")
	}

	go k8s.UpdateCNPStatus(cnpClient, k8s.BackOffLoopTimeout, ciliumRulesStore, rule, cnpns)
}

func (d *Daemon) deleteCiliumNetworkPolicy(obj interface{}) {
	rule, ok := obj.(*k8s.CiliumNetworkPolicy)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).Warn("Received unknown object, expected a CiliumNetworkPolicy object")
		return
	}

	scopedLog := log.WithFields(log.Fields{
		logfields.CiliumNetworkPolicyName: rule.Metadata.Name,
	})
	scopedLog.WithField(logfields.CiliumNetworkPolicy, logfields.Repr(rule)).Debug("Deleting CiliumNetworkPolicy")

	rules, err := rule.Parse()
	if err == nil {
		if len(rules) > 0 {
			_, err = d.PolicyDelete(rules[0].Labels)
		}
	}

	if err != nil {
		scopedLog.WithError(err).Warn("Unable to delete CiliumNetworkPolicy")
	} else {
		scopedLog.Info("Deleted CiliumNetworkPolicy")
	}
}

func (d *Daemon) updateCiliumNetworkPolicy(oldObj interface{}, newObj interface{}) {
	oldRule, ok := oldObj.(*k8s.CiliumNetworkPolicy)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(oldObj)).Warn("Received unknown object, expected a CiliumNetworkPolicy object")
		return
	}
	newRules, ok := newObj.(*k8s.CiliumNetworkPolicy)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(newObj)).Warn("Received unknown object, expected a CiliumNetworkPolicy object")
		return
	}
	// Since we are updating the status map from all nodes we need to prevent
	// deletion and addition of all rules in cilium.
	if oldRule.SpecEquals(newRules) {
		return
	}

	d.deleteCiliumNetworkPolicy(oldObj)
	d.addCiliumNetworkPolicy(newObj)
}

func (d *Daemon) nodesAddFn(obj interface{}) {
	k8sNode, ok := obj.(*v1.Node)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).Warn("Invalid objected, expected v1.Node")
		return
	}
	ni := node.Identity{Name: k8sNode.Name}
	n := k8s.ParseNode(k8sNode)

	routeTypes := node.TunnelRoute

	// Add IPv6 routing only in non encap. With encap we do it with bpf tunnel
	// FIXME create a function to know on which mode is the daemon running on
	var ownAddr net.IP
	if autoIPv6NodeRoutes && d.conf.Device != "undefined" {
		// ignore own node
		if n.Name != nodeaddress.GetName() {
			ownAddr = nodeaddress.GetIPv6()
			routeTypes |= node.DirectRoute
		}
	}

	node.UpdateNode(ni, n, routeTypes, ownAddr)

	log.WithFields(log.Fields{
		logfields.K8sNodeID: ni,
		logfields.Node:      logfields.Repr(n),
	}).Debug("Added node")
}

func (d *Daemon) nodesModFn(oldObj interface{}, newObj interface{}) {
	k8sNode, ok := newObj.(*v1.Node)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(newObj)).Warn("Invalid objected, expected v1.Node")
		return
	}

	newNode := k8s.ParseNode(k8sNode)
	ni := node.Identity{Name: k8sNode.Name}

	oldNode := node.GetNode(ni)

	// If node is the same don't even change it on the map
	// TODO: Run the DeepEqual only for the metadata that we care about?
	if reflect.DeepEqual(oldNode, newNode) {
		return
	}

	routeTypes := node.TunnelRoute
	// Always re-add the routing tables as they might be accidentally removed
	var ownAddr net.IP
	if autoIPv6NodeRoutes && d.conf.Device != "undefined" {
		// ignore own node
		if newNode.Name != nodeaddress.GetName() {
			ownAddr = nodeaddress.GetIPv6()
			routeTypes |= node.DirectRoute
		}
	}

	node.UpdateNode(ni, newNode, routeTypes, ownAddr)

	log.WithFields(log.Fields{
		logfields.K8sNodeID: ni,
		logfields.Node:      logfields.Repr(newNode),
	}).Debug("k8s: Updated node")
}

func (d *Daemon) nodesDelFn(obj interface{}) {
	k8sNode, ok := obj.(*v1.Node)
	if !ok {
		log.WithField(logfields.Object, logfields.Repr(obj)).Warn("Invalid objected, expected v1.Node")
		return
	}

	ni := node.Identity{Name: k8sNode.Name}

	node.DeleteNode(ni, node.TunnelRoute|node.DirectRoute)

	log.WithField(logfields.K8sNodeID, ni).Debug("k8s: Removed node")
}
