/*-
* Copyright (c) 2016-2021, F5 Networks, Inc.
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*
*    http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
 */

package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/intstr"

	ficV1 "github.com/F5Networks/f5-ipam-controller/pkg/ipamapis/apis/fic/v1"
	cisapiv1 "github.com/F5Networks/k8s-bigip-ctlr/v2/config/apis/cis/v1"
	log "github.com/F5Networks/k8s-bigip-ctlr/v2/pkg/vlogger"
	routeapi "github.com/openshift/api/route/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

const nginxMonitorPort int32 = 8081

const (
	NotEnabled = iota
	InvalidInput
	NotRequested
	Requested
	Allocated
)

// nextGenResourceWorker starts the Custom Resource Worker.
func (ctlr *Controller) nextGenResourceWorker() {
	log.Debugf("Starting Custom Resource Worker")
	ctlr.setInitialServiceCount()
	ctlr.migrateIPAM()
	if ctlr.mode == OpenShiftMode {
		ctlr.processGlobalExtendedRouteConfig()
	}
	for ctlr.processResources() {
	}
}

func (ctlr *Controller) setInitialServiceCount() {
	var svcCount int
	for _, ns := range ctlr.getWatchingNamespaces() {
		comInf, found := ctlr.getNamespacedCommonInformer(ns)
		if !found {
			continue
		}
		services, err := comInf.svcInformer.GetIndexer().ByIndex("namespace", ns)
		if err != nil {
			continue
		}
		for _, obj := range services {
			svc := obj.(*v1.Service)
			if _, ok := K8SCoreServices[svc.Name]; ok {
				continue
			}
			if svc.Spec.Type != v1.ServiceTypeExternalName {
				svcCount++
			}
		}
	}
	ctlr.initialSvcCount = svcCount
}

// processResources gets resources from the resourceQueue and processes the resource
// depending  on its kind.
func (ctlr *Controller) processResources() bool {

	key, quit := ctlr.resourceQueue.Get()
	if quit {
		// The controller is shutting down.
		log.Debugf("Resource Queue is empty, Going to StandBy Mode")
		return false
	}
	var isRetryableError bool

	defer ctlr.resourceQueue.Done(key)
	rKey := key.(*rqKey)
	log.Debugf("Processing Key: %v", rKey)

	// During Init time, just accumulate all the poolMembers by processing only services
	if ctlr.initState && rKey.kind != Namespace {
		if rKey.kind != Service {
			ctlr.resourceQueue.AddRateLimited(key)
			return true
		}
		ctlr.initialSvcCount--
		if ctlr.initialSvcCount <= 0 {
			ctlr.initState = false
		}
	}

	rscDelete := false
	if rKey.event == Delete {
		rscDelete = true
	}

	// Check the type of resource and process accordingly.
	switch rKey.kind {
	case Route:
		if ctlr.mode != OpenShiftMode {
			break
		}
		route := rKey.rsc.(*routeapi.Route)
		// processRoutes knows when to delete a VS (in the event of global config update and route delete)
		// so should not trigger delete from here
		if rKey.event == Create {
			if _, ok := ctlr.resources.processedNativeResources[resourceRef{
				kind:      Route,
				name:      route.Name,
				namespace: route.Namespace,
			}]; ok {
				break
			}
		}

		if rscDelete {
			delete(ctlr.resources.processedNativeResources, resourceRef{
				kind:      Route,
				namespace: route.Namespace,
				name:      route.Name,
			})
			// Delete the route entry from hostPath Map
			ctlr.deleteHostPathMapEntry(route)
		}
		if routeGroup, ok := ctlr.resources.invertedNamespaceLabelMap[route.Namespace]; ok {
			err := ctlr.processRoutes(routeGroup, false)
			if err != nil {
				// TODO
				utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
				isRetryableError = true
			}
		}

	case ConfigMap:
		if ctlr.mode != OpenShiftMode {
			break
		}
		cm := rKey.rsc.(*v1.ConfigMap)
		err, ok := ctlr.processConfigMap(cm, rscDelete)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
			break
		}

		if !ok {
			isRetryableError = true
		}
	case VirtualServer:
		if ctlr.mode == OpenShiftMode || ctlr.mode == KubernetesMode {
			break
		}
		virtual := rKey.rsc.(*cisapiv1.VirtualServer)
		rscRefKey := resourceRef{
			kind:      VirtualServer,
			name:      virtual.Name,
			namespace: virtual.Namespace,
		}
		if _, ok := ctlr.resources.processedNativeResources[rscRefKey]; ok {
			if rKey.event == Create {
				break
			}
			if rKey.event == Delete {
				delete(ctlr.resources.processedNativeResources, rscRefKey)
			}
		}

		err := ctlr.processVirtualServers(virtual, rscDelete)
		if err != nil {
			// TODO
			utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
			isRetryableError = true
		}
	case TLSProfile:
		if ctlr.mode == OpenShiftMode || ctlr.mode == KubernetesMode {
			break
		}
		tlsProfile := rKey.rsc.(*cisapiv1.TLSProfile)
		virtuals := ctlr.getVirtualsForTLSProfile(tlsProfile)
		// No Virtuals are effected with the change in TLSProfile.
		if nil == virtuals {
			break
		}
		for _, virtual := range virtuals {
			err := ctlr.processVirtualServers(virtual, false)
			if err != nil {
				// TODO
				utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
				isRetryableError = true
			}
		}
	case K8sSecret:
		secret := rKey.rsc.(*v1.Secret)
		switch ctlr.mode {
		case OpenShiftMode:
			routeGroup := ctlr.getRouteGroupForSecret(secret)
			if routeGroup != "" {
				ctlr.processRoutes(routeGroup, false)
			}
		default:
			tlsProfiles := ctlr.getTLSProfilesForSecret(secret)
			for _, tlsProfile := range tlsProfiles {
				virtuals := ctlr.getVirtualsForTLSProfile(tlsProfile)
				// No Virtuals are effected with the change in TLSProfile.
				if nil == virtuals {
					break
				}
				for _, virtual := range virtuals {
					err := ctlr.processVirtualServers(virtual, false)
					if err != nil {
						// TODO
						utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
						isRetryableError = true
					}
				}
			}
		}

	case TransportServer:
		if ctlr.mode == OpenShiftMode || ctlr.mode == KubernetesMode {
			break
		}
		virtual := rKey.rsc.(*cisapiv1.TransportServer)
		err := ctlr.processTransportServers(virtual, rscDelete)
		if err != nil {
			// TODO
			utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
			isRetryableError = true
		}
	case IngressLink:
		if ctlr.mode == OpenShiftMode || ctlr.mode == KubernetesMode {
			break
		}
		ingLink := rKey.rsc.(*cisapiv1.IngressLink)
		log.Infof("Worker got IngressLink: %v\n", ingLink)
		log.Infof("IngressLink Selector: %v\n", ingLink.Spec.Selector.String())
		err := ctlr.processIngressLink(ingLink, rscDelete)
		if err != nil {
			// TODO
			utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
			isRetryableError = true
		}
	case ExternalDNS:
		if ctlr.mode == KubernetesMode {
			break
		}
		edns := rKey.rsc.(*cisapiv1.ExternalDNS)
		ctlr.processExternalDNS(edns, rscDelete)
	case IPAM:
		ipam := rKey.rsc.(*ficV1.IPAM)
		_ = ctlr.processIPAM(ipam)

	case CustomPolicy:
		cp := rKey.rsc.(*cisapiv1.Policy)
		switch ctlr.mode {
		case OpenShiftMode:
			routeGroups := ctlr.getRouteGroupForCustomPolicy(cp.Namespace + "/" + cp.Name)
			for _, routeGroup := range routeGroups {
				_ = ctlr.processRoutes(routeGroup, false)
			}
		default:
			virtuals := ctlr.getVirtualsForCustomPolicy(cp)
			//Sync Custompolicy for Virtual Servers
			for _, virtual := range virtuals {
				err := ctlr.processVirtualServers(virtual, false)
				if err != nil {
					// TODO
					utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
					isRetryableError = true
				}
			}
			//Sync Custompolicy for Transport Servers
			tsVirtuals := ctlr.getTransportServersForCustomPolicy(cp)
			for _, virtual := range tsVirtuals {
				err := ctlr.processTransportServers(virtual, false)
				if err != nil {
					// TODO
					utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
					isRetryableError = true
				}
			}
			//Sync Custompolicy for Services of type LB
			lbServices := ctlr.getLBServicesForCustomPolicy(cp)
			for _, lbService := range lbServices {
				err := ctlr.processLBServices(lbService, false)
				if err != nil {
					// TODO
					utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
					isRetryableError = true
				}
			}
		}
	case Service:
		svc := rKey.rsc.(*v1.Service)

		_ = ctlr.processService(svc, nil, rscDelete)

		if svc.Spec.Type == v1.ServiceTypeLoadBalancer {
			err := ctlr.processLBServices(svc, rscDelete)
			if err != nil {
				// TODO
				utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
				isRetryableError = true
			}
			break
		}
		if ctlr.initState {
			break
		}
		switch ctlr.mode {
		case OpenShiftMode:
			ctlr.updatePoolMembersForRoutes(svc, false)
		default:
			virtuals := ctlr.getVirtualServersForService(svc)
			// If nil No Virtuals are effected with the change in service.
			if nil != virtuals {
				for _, virtual := range virtuals {
					err := ctlr.processVirtualServers(virtual, false)
					if err != nil {
						// TODO
						utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
						isRetryableError = true
					}
				}
			}
			//Sync service for Transport Server virtuals
			tsVirtuals := ctlr.getTransportServersForService(svc)
			if nil != tsVirtuals {
				for _, virtual := range tsVirtuals {
					err := ctlr.processTransportServers(virtual, false)
					if err != nil {
						// TODO
						utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
						isRetryableError = true
					}
				}
			}
			//Sync service for Ingress Links
			ingLinks := ctlr.getIngressLinksForService(svc)
			if nil != ingLinks {
				for _, ingLink := range ingLinks {
					// Delete/sync IngressLink. Delete will be processed with old service
					err := ctlr.processIngressLink(ingLink, rscDelete)
					if err != nil {
						if rscDelete {
							utilruntime.HandleError(fmt.Errorf("Deleting IngresLink %v failed with %v", ingLink.Name, err))
						} else {
							// TODO
							utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
						}
						isRetryableError = true
					}
				}
			}
		}

	case Endpoints:
		ep := rKey.rsc.(*v1.Endpoints)
		svc := ctlr.getServiceForEndpoints(ep)
		// No Services are effected with the change in service.
		if nil == svc {
			break
		}

		_ = ctlr.processService(svc, ep, rscDelete)

		if svc.Spec.Type == v1.ServiceTypeLoadBalancer {
			err := ctlr.processLBServices(svc, rscDelete)
			if err != nil {
				// TODO
				utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
				isRetryableError = true
			}
			break
		}
		switch ctlr.mode {
		case OpenShiftMode:
			ctlr.updatePoolMembersForRoutes(svc, true)
		default:
			// once we fetch the VS, just update the endpoints instead of processing them entirely
			ctlr.updatePoolMembersForVirtuals(svc)
		}

	case Pod:
		pod := rKey.rsc.(*v1.Pod)
		_ = ctlr.processPod(pod, rscDelete)
		svc := ctlr.GetServicesForPod(pod)
		if nil == svc {
			break
		}
		_ = ctlr.processService(svc, nil, false)
		if svc.Spec.Type == v1.ServiceTypeLoadBalancer {
			err := ctlr.processLBServices(svc, rscDelete)
			if err != nil {
				// TODO
				utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
				isRetryableError = true
			}
			break
		}
		switch ctlr.mode {
		case OpenShiftMode:
			ctlr.updatePoolMembersForRoutes(svc, false)
		default:
			virtuals := ctlr.getVirtualServersForService(svc)
			for _, virtual := range virtuals {
				err := ctlr.processVirtualServers(virtual, false)
				if err != nil {
					// TODO
					utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
					isRetryableError = true
				}
			}
			//Sync service for Transport Server virtuals
			tsVirtuals := ctlr.getTransportServersForService(svc)
			if nil != tsVirtuals {
				for _, virtual := range tsVirtuals {
					err := ctlr.processTransportServers(virtual, false)
					if err != nil {
						// TODO
						utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
						isRetryableError = true
					}
				}
			}
			//Sync service for Ingress Links
			ingLinks := ctlr.getIngressLinksForService(svc)
			if nil != ingLinks {
				for _, ingLink := range ingLinks {
					err := ctlr.processIngressLink(ingLink, false)
					if err != nil {
						// TODO
						utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
						isRetryableError = true
					}
				}
			}
		}

	case Namespace:
		ns := rKey.rsc.(*v1.Namespace)
		nsName := ns.ObjectMeta.Name
		switch ctlr.mode {

		case OpenShiftMode:
			var triggerDelete bool
			if rscDelete {
				// TODO: Delete all the resource configs from the store
				if nrInf, ok := ctlr.nrInformers[nsName]; ok {
					nrInf.stop()
					delete(ctlr.nrInformers, nsName)
				}
				if comInf, ok := ctlr.comInformers[nsName]; ok {
					comInf.stop()
					delete(ctlr.comInformers, nsName)
				}
				ctlr.namespacesMutex.Lock()
				delete(ctlr.namespaces, nsName)
				ctlr.namespacesMutex.Unlock()
				log.Debugf("Removed Namespace: '%v' from CIS scope", nsName)
				triggerDelete = true
			} else {
				ctlr.namespacesMutex.Lock()
				ctlr.namespaces[nsName] = true
				ctlr.namespacesMutex.Unlock()
				_ = ctlr.addNamespacedInformers(nsName, true)
				log.Debugf("Added Namespace: '%v' to CIS scope", nsName)
			}
			if ctlr.namespaceLabelMode {
				ctlr.processGlobalExtendedRouteConfig()
			} else {
				if routeGroup, ok := ctlr.resources.invertedNamespaceLabelMap[nsName]; ok {
					_ = ctlr.processRoutes(routeGroup, triggerDelete)
				}
			}

		default:
			if rscDelete {
				for _, vrt := range ctlr.getAllVirtualServers(nsName) {
					err := ctlr.processVirtualServers(vrt, true)
					if err != nil {
						// TODO
						utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
						isRetryableError = true
					}
				}

				for _, ts := range ctlr.getAllTransportServers(nsName) {
					err := ctlr.processTransportServers(ts, true)
					if err != nil {
						// TODO
						utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
						isRetryableError = true
					}
				}

				ctlr.crInformers[nsName].stop()
				delete(ctlr.crInformers, nsName)
				ctlr.namespacesMutex.Lock()
				delete(ctlr.namespaces, nsName)
				ctlr.namespacesMutex.Unlock()
				log.Debugf("Removed Namespace: '%v' from CIS scope", nsName)
			} else {
				ctlr.namespacesMutex.Lock()
				ctlr.namespaces[nsName] = true
				ctlr.namespacesMutex.Unlock()
				_ = ctlr.addNamespacedInformers(nsName, true)
				log.Debugf("Added Namespace: '%v' to CIS scope", nsName)
			}
		}
	default:
		log.Errorf("Unknown resource Kind: %v", rKey.kind)
	}

	if isRetryableError {
		ctlr.resourceQueue.AddRateLimited(key)
	} else {
		ctlr.resourceQueue.Forget(key)
	}

	if ctlr.resourceQueue.Len() == 0 && ctlr.resources.isConfigUpdated() {
		config := ResourceConfigRequest{
			ltmConfig:          ctlr.resources.getLTMConfigDeepCopy(),
			shareNodes:         ctlr.shareNodes,
			gtmConfig:          ctlr.resources.getGTMConfigCopy(),
			defaultRouteDomain: ctlr.defaultRouteDomain,
		}
		go ctlr.TeemData.PostTeemsData()
		config.reqId = ctlr.enqueueReq(config)
		ctlr.Agent.PostConfig(config)
		ctlr.initState = false
		ctlr.resources.updateCaches()
	}
	return true
}

// getServiceForEndpoints returns the service associated with endpoints.
func (ctlr *Controller) getServiceForEndpoints(ep *v1.Endpoints) *v1.Service {

	svcKey := fmt.Sprintf("%s/%s", ep.Namespace, ep.Name)
	comInf, ok := ctlr.getNamespacedCommonInformer(ep.Namespace)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", ep.Namespace)
		return nil
	}
	svc, exists, err := comInf.svcInformer.GetIndexer().GetByKey(svcKey)
	if err != nil {
		log.Infof("Error fetching service %v from the store: %v", svcKey, err)
		return nil
	}
	if !exists {
		log.Infof("Service %v doesn't exist", svcKey)
		return nil
	}

	return svc.(*v1.Service)
}

func (ctlr *Controller) updatePoolMembersForVirtuals(svc *v1.Service) {

	namespace := svc.Namespace
	svcName := svc.Name
	svcDepRscKey := namespace + "_" + svcName
	partition := ctlr.Partition

	for rsName := range ctlr.getSvcDepResources(svcDepRscKey) {
		rsCfg := ctlr.getVirtualServer(partition, rsName)
		if rsCfg == nil {
			continue
		}

		freshRsCfg := &ResourceConfig{}
		freshRsCfg.copyConfig(rsCfg)

		if ctlr.PoolMemberType == NodePort {
			ctlr.updatePoolMembersForNodePort(freshRsCfg, namespace)
		} else if ctlr.PoolMemberType == NodePortLocal {
			//supported with antrea cni.
			ctlr.updatePoolMembersForNPL(freshRsCfg, namespace)
		} else {
			ctlr.updatePoolMembersForCluster(freshRsCfg, namespace)
		}
		_ = ctlr.resources.setResourceConfig(partition, rsName, freshRsCfg)
	}
}

// getVirtualServersForService gets the List of VirtualServers which are effected
// by the addition/deletion/updation of service.
func (ctlr *Controller) getVirtualServersForService(svc *v1.Service) []*cisapiv1.VirtualServer {

	allVirtuals := ctlr.getAllVirtualServers(svc.ObjectMeta.Namespace)
	if nil == allVirtuals {
		log.Infof("No VirtualServers found in namespace %s",
			svc.ObjectMeta.Namespace)
		return nil
	}

	// find VirtualServers that reference the service
	virtualsForService := filterVirtualServersForService(allVirtuals, svc)
	if nil == virtualsForService {
		log.Debugf("Change in Service %s does not effect any VirtualServer",
			svc.ObjectMeta.Name)
		return nil
	}
	return virtualsForService
}

// getVirtualsForTLSProfile gets the List of VirtualServers which are effected
// by the addition/deletion/updation of TLSProfile.
func (ctlr *Controller) getVirtualsForTLSProfile(tls *cisapiv1.TLSProfile) []*cisapiv1.VirtualServer {

	allVirtuals := ctlr.getAllVirtualServers(tls.ObjectMeta.Namespace)
	if nil == allVirtuals {
		log.Infof("No VirtualServers found in namespace %s",
			tls.ObjectMeta.Namespace)
		return nil
	}

	// find VirtualServers that reference the TLSProfile
	virtualsForTLSProfile := getVirtualServersForTLSProfile(allVirtuals, tls)
	if nil == virtualsForTLSProfile {
		log.Infof("Change in TLSProfile %s does not effect any VirtualServer",
			tls.ObjectMeta.Name)
		return nil
	}
	return virtualsForTLSProfile
}

func (ctlr *Controller) getVirtualsForCustomPolicy(plc *cisapiv1.Policy) []*cisapiv1.VirtualServer {
	nsVirtuals := ctlr.getAllVirtualServers(plc.Namespace)
	if nil == nsVirtuals {
		log.Infof("No VirtualServers found in namespace %s",
			plc.Namespace)
		return nil
	}

	var plcVSs []*cisapiv1.VirtualServer
	var plcVSNames []string
	for _, vs := range nsVirtuals {
		if vs.Spec.PolicyName == plc.Name {
			plcVSs = append(plcVSs, vs)
			plcVSNames = append(plcVSNames, vs.Name)
		}
	}

	log.Debugf("VirtualServers %v are affected with Custom Policy %s: ",
		plcVSNames, plc.Name)

	return plcVSs
}

func (ctlr *Controller) getTransportServersForCustomPolicy(plc *cisapiv1.Policy) []*cisapiv1.TransportServer {
	nsVirtuals := ctlr.getAllTransportServers(plc.Namespace)
	if nil == nsVirtuals {
		log.Infof("No VirtualServers found in namespace %s",
			plc.Namespace)
		return nil
	}

	var plcVSs []*cisapiv1.TransportServer
	var plcVSNames []string
	for _, vs := range nsVirtuals {
		if vs.Spec.PolicyName == plc.Name {
			plcVSs = append(plcVSs, vs)
			plcVSNames = append(plcVSNames, vs.Name)
		}
	}

	log.Debugf("VirtualServers %v are affected with Custom Policy %s: ",
		plcVSNames, plc.Name)

	return plcVSs
}

// getLBServicesForCustomPolicy gets all services of type LB affected by the policy
func (ctlr *Controller) getLBServicesForCustomPolicy(plc *cisapiv1.Policy) []*v1.Service {
	LBServices := ctlr.getAllLBServices(plc.Namespace)
	if nil == LBServices {
		log.Infof("No LB service found in namespace %s",
			plc.Namespace)
		return nil
	}

	var plcSvcs []*v1.Service
	var plcSvcNames []string
	for _, svc := range LBServices {
		if plcName, found := svc.Annotations[LBServicePolicyNameAnnotation]; found && plcName == plc.Name {
			plcSvcs = append(plcSvcs, svc)
			plcSvcNames = append(plcSvcNames, svc.Name)
		}
	}

	log.Debugf("LB Services %v are affected with Custom Policy %s: ",
		plcSvcNames, plc.Name)

	return plcSvcs
}

// getAllVirtualServers returns list of all valid VirtualServers in rkey namespace.
func (ctlr *Controller) getAllVirtualServers(namespace string) []*cisapiv1.VirtualServer {
	var allVirtuals []*cisapiv1.VirtualServer

	crInf, ok := ctlr.getNamespacedCRInformer(namespace)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", namespace)
		return nil
	}

	var orderedVSs []interface{}
	var err error
	if namespace == "" {
		orderedVSs = crInf.vsInformer.GetIndexer().List()
	} else {
		// Get list of VirtualServers and process them.
		orderedVSs, err = crInf.vsInformer.GetIndexer().ByIndex("namespace", namespace)
		if err != nil {
			log.Errorf("Unable to get list of VirtualServers for namespace '%v': %v",
				namespace, err)
			return nil
		}
	}

	for _, obj := range orderedVSs {
		vs := obj.(*cisapiv1.VirtualServer)
		// TODO: Validate the VirtualServers List to check if all the vs are valid.
		allVirtuals = append(allVirtuals, vs)
	}

	return allVirtuals
}

// getAllVirtualServers returns list of all valid VirtualServers in rkey namespace.
func (ctlr *Controller) getAllVSFromMonitoredNamespaces() []*cisapiv1.VirtualServer {
	var allVirtuals []*cisapiv1.VirtualServer
	if ctlr.watchingAllNamespaces() {
		return ctlr.getAllVirtualServers("")
	}
	for ns := range ctlr.namespaces {
		allVirtuals = append(allVirtuals, ctlr.getAllVirtualServers(ns)...)
	}

	return allVirtuals
}

// filterVirtualServersForService returns list of VirtualServers that are
// affected by the service under process.
func filterVirtualServersForService(allVirtuals []*cisapiv1.VirtualServer,
	svc *v1.Service) []*cisapiv1.VirtualServer {

	var result []*cisapiv1.VirtualServer
	svcName := svc.ObjectMeta.Name
	svcNamespace := svc.ObjectMeta.Namespace

	for _, vs := range allVirtuals {
		if vs.ObjectMeta.Namespace != svcNamespace {
			continue
		}

		isValidVirtual := false
		for _, pool := range vs.Spec.Pools {
			if pool.Service == svcName {
				isValidVirtual = true
				break
			}
		}
		if !isValidVirtual {
			continue
		}

		result = append(result, vs)
	}

	return result
}

// getVirtualServersForTLS returns list of VirtualServers that are
// affected by the TLSProfile under process.
func getVirtualServersForTLSProfile(allVirtuals []*cisapiv1.VirtualServer,
	tls *cisapiv1.TLSProfile) []*cisapiv1.VirtualServer {

	var result []*cisapiv1.VirtualServer
	tlsName := tls.ObjectMeta.Name
	tlsNamespace := tls.ObjectMeta.Namespace

	for _, vs := range allVirtuals {
		if vs.ObjectMeta.Namespace == tlsNamespace && vs.Spec.TLSProfileName == tlsName {
			found := false
			for _, host := range tls.Spec.Hosts {
				if vs.Spec.Host == host {
					result = append(result, vs)
					found = true
					break
				}
			}
			if !found {
				log.Errorf("TLSProfile hostname is not same as virtual host %s for profile %s", vs.Spec.Host, vs.Spec.TLSProfileName)
			}
		}
	}

	return result
}

func (ctlr *Controller) getTLSProfileForVirtualServer(
	vs *cisapiv1.VirtualServer,
	namespace string) *cisapiv1.TLSProfile {
	tlsName := vs.Spec.TLSProfileName
	tlsKey := fmt.Sprintf("%s/%s", namespace, tlsName)

	// Initialize CustomResource Informer for required namespace
	crInf, ok := ctlr.getNamespacedCRInformer(namespace)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", namespace)
		return nil
	}
	comInf, ok := ctlr.getNamespacedCommonInformer(namespace)
	if !ok {
		log.Errorf("Common Informer not found for namespace: %v", namespace)
		return nil
	}
	// TODO: Create Internal Structure to hold TLSProfiles. Make API call only for a new TLSProfile
	// Check if the TLSProfile exists and valid for us.
	obj, tlsFound, _ := crInf.tlsInformer.GetIndexer().GetByKey(tlsKey)
	if !tlsFound {
		log.Errorf("TLSProfile %s does not exist", tlsName)
		return nil
	}

	// validate TLSProfile
	validation := validateTLSProfile(obj.(*cisapiv1.TLSProfile))
	if validation == false {
		return nil
	}

	tlsProfile := obj.(*cisapiv1.TLSProfile)

	if tlsProfile.Spec.TLS.Reference == "secret" {
		var match bool
		if len(tlsProfile.Spec.TLS.ClientSSLs) > 0 {
			for _, secret := range tlsProfile.Spec.TLS.ClientSSLs {
				secretKey := namespace + "/" + secret
				clientSecretobj, found, err := comInf.secretsInformer.GetIndexer().GetByKey(secretKey)
				if err != nil || !found {
					return nil
				}
				clientSecret := clientSecretobj.(*v1.Secret)
				//validate at least one clientSSL certificates matches the VS hostname
				if checkCertificateHost(vs.Spec.Host, clientSecret.Data["tls.crt"], clientSecret.Data["tls.key"]) {
					match = true
					break
				}
			}

		} else {
			secretKey := namespace + "/" + tlsProfile.Spec.TLS.ClientSSL
			clientSecretobj, found, err := comInf.secretsInformer.GetIndexer().GetByKey(secretKey)
			if err != nil || !found {
				return nil
			}
			clientSecret := clientSecretobj.(*v1.Secret)
			//validate clientSSL certificates and hostname
			match = checkCertificateHost(vs.Spec.Host, clientSecret.Data["tls.crt"], clientSecret.Data["tls.key"])
		}
		if match == false {
			return nil
		}
	}
	if len(vs.Spec.Host) == 0 {
		// VirtualServer without host may be used for group of services
		// which are common amongst multiple hosts. Example: Error Page
		// application may be common for multiple hosts.
		// However, each host use a unique TLSProfile w.r.t SNI
		return tlsProfile
	}

	for _, host := range tlsProfile.Spec.Hosts {
		if host == vs.Spec.Host {
			// TLSProfile Object
			return tlsProfile
		}
		// check for wildcard match
		if strings.HasPrefix(host, "*") {
			host = strings.TrimPrefix(host, "*")
			if strings.HasSuffix(vs.Spec.Host, host) {
				// TLSProfile Object
				return tlsProfile
			}
		}
	}
	log.Errorf("TLSProfile %s with host %s does not match with virtual server %s host.", tlsName, vs.Spec.Host, vs.ObjectMeta.Name)
	return nil

}

func isTLSVirtualServer(vrt *cisapiv1.VirtualServer) bool {
	return len(vrt.Spec.TLSProfileName) != 0
}

func doesVSHandleHTTP(vrt *cisapiv1.VirtualServer) bool {
	if !isTLSVirtualServer(vrt) {
		// If it is not TLS VirtualServer(HTTPS), then it is HTTP server
		return true
	}
	// If Allow or Redirect happens then HTTP Traffic is being handled.
	return vrt.Spec.HTTPTraffic == TLSAllowInsecure ||
		vrt.Spec.HTTPTraffic == TLSRedirectInsecure
}

// doVSHandleHTTP checks if any of the associated vituals handle HTTP traffic and use same port
func doVSHandleHTTP(virtuals []*cisapiv1.VirtualServer, virtual *cisapiv1.VirtualServer) bool {
	effectiveHTTPPort := getEffectiveHTTPPort(virtual)
	for _, vrt := range virtuals {
		if doesVSHandleHTTP(vrt) && effectiveHTTPPort == getEffectiveHTTPPort(vrt) {
			return true
		}
	}
	return false
}

// processVirtualServers takes the Virtual Server as input and processes all
// associated VirtualServers to create a resource config(Internal DataStructure)
// or to update if exists already.
func (ctlr *Controller) processVirtualServers(
	virtual *cisapiv1.VirtualServer,
	isVSDeleted bool,
) error {

	startTime := time.Now()
	defer func() {
		endTime := time.Now()
		log.Debugf("Finished syncing virtual servers %+v (%v)",
			virtual, endTime.Sub(startTime))
	}()

	// Skip validation for a deleted Virtual Server
	if !isVSDeleted {
		// check if the virutal server matches all the requirements.
		vkey := virtual.ObjectMeta.Namespace + "/" + virtual.ObjectMeta.Name
		valid := ctlr.checkValidVirtualServer(virtual)
		if false == valid {
			log.Errorf("VirtualServer %s, is not valid",
				vkey)
			return nil
		}
	}

	var allVirtuals []*cisapiv1.VirtualServer
	if virtual.Spec.HostGroup != "" {
		// grouping by hg across all namespaces
		allVirtuals = ctlr.getAllVSFromMonitoredNamespaces()
	} else {
		allVirtuals = ctlr.getAllVirtualServers(virtual.ObjectMeta.Namespace)
	}
	ctlr.TeemData.Lock()
	ctlr.TeemData.ResourceType.VirtualServer[virtual.ObjectMeta.Namespace] = len(allVirtuals)
	ctlr.TeemData.Unlock()

	// Prepare list of associated VirtualServers to be processed
	// In the event of deletion, exclude the deleted VirtualServer
	log.Debugf("Process all the Virtual Servers which share same VirtualServerAddress")

	virtuals := ctlr.getAssociatedVirtualServers(virtual, allVirtuals, isVSDeleted)

	var ip string
	var status int
	if ctlr.ipamCli != nil {
		if isVSDeleted && len(virtuals) == 0 && virtual.Spec.VirtualServerAddress == "" {
			if virtual.Spec.HostGroup != "" {
				//hg is unique across namespaces
				//all virtuals with same hg are grouped together across namespaces
				key := virtual.Spec.HostGroup + "_hg"
				ip = ctlr.releaseIP(virtual.Spec.IPAMLabel, "", key)
			} else {
				key := virtual.Namespace + "/" + virtual.Spec.Host + "_host"
				ip = ctlr.releaseIP(virtual.Spec.IPAMLabel, virtual.Spec.Host, key)
			}
		} else if virtual.Spec.VirtualServerAddress != "" {
			// Prioritise VirtualServerAddress specified over IPAMLabel
			ip = virtual.Spec.VirtualServerAddress
		} else {
			ipamLabel := getIPAMLabel(virtuals)
			if virtual.Spec.HostGroup != "" {
				//hg is unique across namepsaces
				key := virtual.Spec.HostGroup + "_hg"
				ip, status = ctlr.requestIP(ipamLabel, "", key)
			} else {
				key := virtual.Namespace + "/" + virtual.Spec.Host + "_host"
				ip, status = ctlr.requestIP(ipamLabel, virtual.Spec.Host, key)
			}

			switch status {
			case NotEnabled:
				log.Debug("IPAM Custom Resource Not Available")
				return nil
			case InvalidInput:
				log.Debugf("IPAM Invalid IPAM Label: %v for Virtual Server: %s/%s", ipamLabel, virtual.Namespace, virtual.Name)
				return nil
			case NotRequested:
				return fmt.Errorf("unable make do IPAM Request, will be re-requested soon")
			case Requested:
				log.Debugf("IP address requested for service: %s/%s", virtual.Namespace, virtual.Name)
				return nil
			}
			virtual.Status.VSAddress = ip
		}
	} else {
		if virtual.Spec.HostGroup == "" {
			if virtual.Spec.VirtualServerAddress == "" {
				return fmt.Errorf("No VirtualServer address or IPAM found.")
			}
			ip = virtual.Spec.VirtualServerAddress
		} else {
			var err error
			ip, err = getVirtualServerAddress(virtuals)
			if err != nil {
				log.Errorf("Error in virtualserver address: %s", err.Error())
				return err
			}
			if ip == "" {
				ip = virtual.Spec.VirtualServerAddress
				if ip == "" {
					return fmt.Errorf("No VirtualServer address found for: %s", virtual.Name)
				}
			}
		}
	}
	// Depending on the ports defined, TLS type or Unsecured we will populate the resource config.
	portStructs := ctlr.virtualPorts(virtual)

	// vsMap holds Resource Configs of current virtuals temporarily
	vsMap := make(ResourceMap)
	processingError := false
	for _, portStruct := range portStructs {
		// TODO: Add Route Domain
		var rsName string
		if virtual.Spec.VirtualServerName != "" {
			rsName = formatCustomVirtualServerName(
				virtual.Spec.VirtualServerName,
				portStruct.port,
			)
		} else {
			rsName = formatVirtualServerName(
				ip,
				portStruct.port,
			)
		}

		// Delete rsCfg if no corresponding virtuals exist
		// Delete rsCfg if it is HTTP rsCfg and the CR VirtualServer does not handle HTTPTraffic
		if (len(virtuals) == 0) ||
			(portStruct.protocol == HTTP && !doVSHandleHTTP(virtuals, virtual)) ||
			(isVSDeleted && portStruct.protocol == HTTPS && !doVSUseSameHTTPSPort(virtuals, virtual)) {
			var hostnames []string
			rsMap := ctlr.resources.getPartitionResourceMap(ctlr.Partition)

			if _, ok := rsMap[rsName]; ok {
				hostnames = rsMap[rsName].MetaData.hosts
			}
			ctlr.deleteSvcDepResource(rsName, rsMap[rsName])
			ctlr.deleteVirtualServer(ctlr.Partition, rsName)
			if len(hostnames) > 0 {
				ctlr.ProcessAssociatedExternalDNS(hostnames)
			}
			continue
		}

		rsCfg := &ResourceConfig{}
		rsCfg.Virtual.Partition = ctlr.Partition
		rsCfg.MetaData.ResourceType = VirtualServer
		rsCfg.Virtual.Enabled = true
		rsCfg.Virtual.Name = rsName
		rsCfg.MetaData.hosts = append(rsCfg.MetaData.hosts, virtual.Spec.Host)
		rsCfg.MetaData.Protocol = portStruct.protocol
		rsCfg.MetaData.httpTraffic = virtual.Spec.HTTPTraffic
		rsCfg.Virtual.HttpMrfRoutingEnabled = virtual.Spec.HttpMrfRoutingEnabled
		rsCfg.MetaData.baseResources = make(map[string]string)
		rsCfg.Virtual.SetVirtualAddress(
			ip,
			portStruct.port,
		)
		rsCfg.IntDgMap = make(InternalDataGroupMap)
		rsCfg.IRulesMap = make(IRulesMap)
		rsCfg.customProfiles = make(map[SecretKey]CustomProfile)

		plc, err := ctlr.getPolicyFromVirtuals(virtuals)
		if plc != nil {
			err := ctlr.handleVSResourceConfigForPolicy(rsCfg, plc)
			if err != nil {
				processingError = true
				break
			}
		}
		if err != nil {
			processingError = true
			log.Errorf("%v", err)
			break
		}

		for _, vrt := range virtuals {
			passthroughVS := false
			var tlsProf *cisapiv1.TLSProfile
			if isTLSVirtualServer(vrt) {
				// Handle TLS configuration for VirtualServer Custom Resource
				tlsProf = ctlr.getTLSProfileForVirtualServer(vrt, vrt.Namespace)
				if tlsProf == nil {
					// Processing failed
					// Stop processing further virtuals
					processingError = true
					break
				}
				if tlsProf.Spec.TLS.Termination == TLSPassthrough {
					passthroughVS = true
				}
			}

			log.Debugf("Processing Virtual Server %s for port %v",
				vrt.ObjectMeta.Name, portStruct.port)
			rsCfg.MetaData.baseResources[vrt.Namespace+"/"+vrt.Name] = VirtualServer
			err := ctlr.prepareRSConfigFromVirtualServer(
				rsCfg,
				vrt,
				passthroughVS,
			)
			if err != nil {
				processingError = true
				break
			}

			if tlsProf != nil {
				processed := ctlr.handleVirtualServerTLS(rsCfg, vrt, tlsProf, ip)
				if !processed {
					// Processing failed
					// Stop processing further virtuals
					processingError = true
					break
				}

				log.Debugf("Updated Virtual %s with TLSProfile %s",
					vrt.ObjectMeta.Name, vrt.Spec.TLSProfileName)
			}

			ctlr.updateSvcDepResources(rsName, rsCfg)

			ctlr.resources.processedNativeResources[resourceRef{
				kind:      VirtualServer,
				namespace: vrt.Namespace,
				name:      vrt.Name,
			}] = struct{}{}

		}

		if processingError {
			log.Errorf("Cannot Publish VirtualServer %s", virtual.ObjectMeta.Name)
			break
		}

		// Save ResourceConfig in temporary Map
		vsMap[rsName] = rsCfg

		if ctlr.PoolMemberType == NodePort {
			ctlr.updatePoolMembersForNodePort(rsCfg, virtual.ObjectMeta.Namespace)
		} else if ctlr.PoolMemberType == NodePortLocal {
			//supported with antrea cni.
			ctlr.updatePoolMembersForNPL(rsCfg, virtual.ObjectMeta.Namespace)
		} else {
			ctlr.updatePoolMembersForCluster(rsCfg, virtual.ObjectMeta.Namespace)
		}
	}

	if !processingError {
		var hostnames []string
		rsMap := ctlr.resources.getPartitionResourceMap(ctlr.Partition)

		// Update ltmConfig with ResourceConfigs created for the current virtuals
		for rsName, rsCfg := range vsMap {
			if _, ok := rsMap[rsName]; !ok {
				hostnames = rsCfg.MetaData.hosts
			}
			rsMap[rsName] = rsCfg
		}

		if len(hostnames) > 0 {
			ctlr.ProcessAssociatedExternalDNS(hostnames)
		}
	}

	return nil
}

// getEffectiveHTTPPort returns the final HTTP port considered for virtual server
func getEffectiveHTTPSPort(vrt *cisapiv1.VirtualServer) int32 {
	effectiveHTTPSPort := DEFAULT_HTTPS_PORT
	if vrt.Spec.VirtualServerHTTPSPort != 0 {
		effectiveHTTPSPort = vrt.Spec.VirtualServerHTTPSPort
	}
	return effectiveHTTPSPort
}

// getEffectiveHTTPPort returns the final HTTP port considered for virtual server
func getEffectiveHTTPPort(vrt *cisapiv1.VirtualServer) int32 {
	effectiveHTTPPort := DEFAULT_HTTP_PORT
	if vrt.Spec.VirtualServerHTTPPort != 0 {
		effectiveHTTPPort = vrt.Spec.VirtualServerHTTPPort
	}
	return effectiveHTTPPort
}

func (ctlr *Controller) getAssociatedVirtualServers(
	currentVS *cisapiv1.VirtualServer,
	allVirtuals []*cisapiv1.VirtualServer,
	isVSDeleted bool,
) []*cisapiv1.VirtualServer {
	// Associated VirutalServers are grouped based on "hostGroup" parameter
	// if hostGroup parameter is not available, they will be grouped on "host" parameter
	// if "host" parameter is not available, they will be grouped on "VirtualServerAddress"

	// The VirtualServers that are being grouped by "hostGroup" or "host" should obey below rules,
	// otherwise the grouping would be treated as invalid and the associatedVirtualServers will be nil.
	//		* all of them should have same "ipamLabel"
	//      * if no "ipamLabel" present, should have same "VirtualServerAddress"
	//
	// However, there are some parameters that are not as stringent as above.
	// which include
	// 		* "VirtualServerHTTPPort" to be same across the group of VirtualServers
	//      * "VirtualServerHTTPSPort" to be same across the group of VirtualServers
	//      * unique paths for a given host name
	// If one (or multiple) of the above parameters are specified in wrong manner in any VirtualServer,
	// that particular VirtualServer will be skipped.

	var virtuals []*cisapiv1.VirtualServer
	// {hostname: {path: <empty_struct>}}
	uniqueHostPathMap := make(map[string]map[string]struct{})

	for _, vrt := range allVirtuals {
		// skip the deleted virtual in the event of deletion
		if isVSDeleted && vrt.Name == currentVS.Name {
			continue
		}

		// skip the virtuals in other HostGroups
		if vrt.Spec.HostGroup != currentVS.Spec.HostGroup {
			continue
		}

		if currentVS.Spec.HostGroup == "" {
			// in the absence of HostGroup, skip the virtuals with other host name
			if vrt.Spec.Host != currentVS.Spec.Host {
				continue
			}

			// Same host with different VirtualServerAddress is invalid
			if vrt.Spec.VirtualServerAddress != currentVS.Spec.VirtualServerAddress {
				if vrt.Spec.Host != "" {
					log.Errorf("Same host %v is configured with different VirtualServerAddress : %v ", vrt.Spec.Host, vrt.Spec.VirtualServerName)
					return nil
				}
				// In case of empty host name, skip the virtual with other VirtualServerAddress
				continue
			}
		}

		if ctlr.ipamCli != nil {
			if currentVS.Spec.HostGroup == "" && vrt.Spec.IPAMLabel != currentVS.Spec.IPAMLabel {
				log.Errorf("Same host %v is configured with different IPAM labels: %v, %v. Unable to process %v", vrt.Spec.Host, vrt.Spec.IPAMLabel, currentVS.Spec.IPAMLabel, currentVS.Name)
				return nil
			}
			// Empty host with IPAM label is invalid for a Virtual Server
			if vrt.Spec.IPAMLabel != "" && vrt.Spec.Host == "" {
				log.Errorf("Hostless VS %v is configured with IPAM label: %v", vrt.ObjectMeta.Name, vrt.Spec.IPAMLabel)
				return nil
			}
		}

		// skip the virtuals with different custom HTTP/HTTPS ports
		if skipVirtual(currentVS, vrt) {
			continue
		}

		// Check for duplicate path entries among virtuals
		uniquePaths, ok := uniqueHostPathMap[vrt.Spec.Host]
		if !ok {
			uniqueHostPathMap[vrt.Spec.Host] = make(map[string]struct{})
			uniquePaths = uniqueHostPathMap[vrt.Spec.Host]
		}
		isUnique := true
		for _, pool := range vrt.Spec.Pools {
			if _, ok := uniquePaths[pool.Path]; ok {
				// path already exists for the same host
				log.Debugf("Discarding the VirtualServer %v/%v due to duplicate path",
					vrt.ObjectMeta.Namespace, vrt.ObjectMeta.Name)
				isUnique = false
				break
			}
			uniquePaths[pool.Path] = struct{}{}
		}
		if isUnique {
			virtuals = append(virtuals, vrt)
		}
	}
	return virtuals
}

func (ctlr *Controller) getPolicyFromVirtuals(virtuals []*cisapiv1.VirtualServer) (*cisapiv1.Policy, error) {

	if len(virtuals) == 0 {
		log.Errorf("No virtuals to extract policy from")
		return nil, nil
	}
	plcName := ""
	ns := virtuals[0].Namespace

	for _, vrt := range virtuals {
		if plcName != "" && vrt.Spec.PolicyName != "" && plcName != vrt.Spec.PolicyName {
			return nil, fmt.Errorf("Multiple Policies specified for host: %v", vrt.Spec.Host)
		}
		if vrt.Spec.PolicyName != "" {
			plcName = vrt.Spec.PolicyName
		}
	}
	if plcName == "" {
		return nil, nil
	}
	crInf, ok := ctlr.getNamespacedCommonInformer(ns)
	if !ok {
		return nil, fmt.Errorf("Informer not found for namespace: %v", ns)
	}

	key := ns + "/" + plcName

	obj, exist, err := crInf.plcInformer.GetIndexer().GetByKey(key)
	if err != nil {
		return nil, fmt.Errorf("Error while fetching Policy: %v: %v", key, err)
	}

	if !exist {
		return nil, fmt.Errorf("Policy Not Found: %v", key)
	}

	return obj.(*cisapiv1.Policy), nil
}

func (ctlr *Controller) getPolicyFromTransportServer(virtual *cisapiv1.TransportServer) (*cisapiv1.Policy, error) {

	if virtual == nil {
		log.Errorf("No virtuals to extract policy from")
		return nil, nil
	}

	plcName := virtual.Spec.PolicyName
	if plcName == "" {
		return nil, nil
	}
	ns := virtual.Namespace
	return ctlr.getPolicy(ns, plcName)
}

// getPolicy fetches the policy CR
func (ctlr *Controller) getPolicy(ns string, plcName string) (*cisapiv1.Policy, error) {
	crInf, ok := ctlr.getNamespacedCommonInformer(ns)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", ns)
		return nil, fmt.Errorf("Informer not found for namespace: %v", ns)
	}
	key := ns + "/" + plcName

	obj, exist, err := crInf.plcInformer.GetIndexer().GetByKey(key)
	if err != nil {
		log.Errorf("Error while fetching Policy: %v: %v",
			key, err)
		return nil, fmt.Errorf("Error while fetching Policy: %v: %v", key, err)
	}

	if !exist {
		log.Errorf("Policy Not Found: %v", key)
		return nil, fmt.Errorf("Policy Not Found: %v", key)
	}
	return obj.(*cisapiv1.Policy), nil
}

func getIPAMLabel(virtuals []*cisapiv1.VirtualServer) string {
	for _, vrt := range virtuals {
		if vrt.Spec.IPAMLabel != "" {
			return vrt.Spec.IPAMLabel
		}
	}
	return ""
}

func getVirtualServerAddress(virtuals []*cisapiv1.VirtualServer) (string, error) {
	vsa := ""
	for _, vrt := range virtuals {
		if vrt.Spec.VirtualServerAddress != "" {
			if vsa == "" || (vsa == vrt.Spec.VirtualServerAddress) {
				vsa = vrt.Spec.VirtualServerAddress
			} else {
				return "", fmt.Errorf("more than one Virtual Server Address Found")
			}
		}
	}
	if len(virtuals) != 0 && vsa == "" {
		return "", fmt.Errorf("no Virtual Server Address Found")
	}
	return vsa, nil
}

func (ctlr *Controller) getIPAMCR() *ficV1.IPAM {
	cr := strings.Split(ctlr.ipamCR, "/")
	if len(cr) != 2 {
		log.Errorf("[ipam] error while retrieving IPAM namespace and name.")
		return nil
	}
	ipamCR, err := ctlr.ipamCli.Get(cr[0], cr[1])
	if err != nil {
		log.Errorf("[ipam] error while retrieving IPAM custom resource.")
		return nil
	}
	return ipamCR
}

func (ctlr *Controller) migrateIPAM() {
	if ctlr.ipamCli == nil {
		return
	}

	ipamCR := ctlr.getIPAMCR()
	if ipamCR == nil {
		return
	}

	var specsToMigrate []ficV1.IPSpec

	for _, spec := range ipamCR.Status.IPStatus {
		idx := strings.LastIndex(spec.Key, "_")
		var rscKind string
		if idx != -1 {
			rscKind = spec.Key[idx+1:]
			switch rscKind {
			case "host", "ts", "il", "svc":
				// This entry is fine, process next entry
				continue
			case "hg":
				//Check for format of hg.if key is of format ns/hostgroup_hg
				//this is stale entry from older version, release ip
				if !strings.Contains(spec.Key, "/") {
					continue
				}
			}
		}
		specsToMigrate = append(specsToMigrate, *spec)
	}

	for _, spec := range specsToMigrate {
		ctlr.releaseIP(spec.IPAMLabel, spec.Host, spec.Key)
	}
}

// Request IPAM for virtual IP address
func (ctlr *Controller) requestIP(ipamLabel string, host string, key string) (string, int) {
	ipamCR := ctlr.getIPAMCR()
	var ip string
	var ipReleased bool
	if ipamCR == nil {
		return "", NotEnabled
	}

	if ipamLabel == "" {
		return "", InvalidInput
	}

	if host != "" {
		//For VS server
		for _, ipst := range ipamCR.Status.IPStatus {
			if ipst.IPAMLabel == ipamLabel && ipst.Host == host {
				// IP will be returned later when availability of corresponding spec is confirmed
				ip = ipst.IP
			}
		}

		for _, hst := range ipamCR.Spec.HostSpecs {
			if hst.Host == host {
				if hst.IPAMLabel == ipamLabel {
					if ip != "" {
						// IP extracted from the corresponding status of the spec
						return ip, Allocated
					}

					// HostSpec is already updated with IPAMLabel and Host but IP not got allocated yet
					return "", Requested
				} else {
					// Different Label for same host, this indicates Label is updated
					// Release the old IP, so that new IP can be requested
					ctlr.releaseIP(hst.IPAMLabel, hst.Host, "")
					ipReleased = true
					break
				}
			}
		}

		if ip != "" && !ipReleased {
			// Status is available for non-existing Spec
			// Let the resource get cleaned up and re request later
			return "", NotRequested
		}

		ipamCR.SetResourceVersion(ipamCR.ResourceVersion)
		ipamCR.Spec.HostSpecs = append(ipamCR.Spec.HostSpecs, &ficV1.HostSpec{
			Host:      host,
			Key:       key,
			IPAMLabel: ipamLabel,
		})
	} else if key != "" {
		//For Transport Server
		for _, ipst := range ipamCR.Status.IPStatus {
			if ipst.IPAMLabel == ipamLabel && ipst.Key == key {
				// IP will be returned later when availability of corresponding spec is confirmed
				ip = ipst.IP
			}
		}

		for _, hst := range ipamCR.Spec.HostSpecs {
			if hst.Key == key {
				if hst.IPAMLabel == ipamLabel {
					if ip != "" {
						// IP extracted from the corresponding status of the spec
						return ip, Allocated
					}

					// HostSpec is already updated with IPAMLabel and Host but IP not got allocated yet
					return "", Requested
				} else {
					// Different Label for same key, this indicates Label is updated
					// Release the old IP, so that new IP can be requested
					ctlr.releaseIP(hst.IPAMLabel, "", hst.Key)
					ipReleased = true
					break
				}
			}
		}

		if ip != "" && !ipReleased {
			// Status is available for non-existing Spec
			// Let the resource get cleaned up and re request later
			return "", NotRequested
		}

		ipamCR.SetResourceVersion(ipamCR.ResourceVersion)
		ipamCR.Spec.HostSpecs = append(ipamCR.Spec.HostSpecs, &ficV1.HostSpec{
			Key:       key,
			IPAMLabel: ipamLabel,
		})
	} else {
		log.Debugf("[IPAM] Invalid host and key.")
		return "", InvalidInput
	}

	_, err := ctlr.ipamCli.Update(ipamCR)
	if err != nil {
		log.Errorf("[ipam] Error updating IPAM CR : %v", err)
		return "", NotRequested
	}

	log.Debugf("[ipam] Updated IPAM CR.")
	return "", Requested

}

// Get List of VirtualServers associated with the IPAM resource
func (ctlr *Controller) VerifyIPAMAssociatedHostGroupExists(key string) bool {
	allTS := ctlr.getAllTSFromMonitoredNamespaces()
	for _, ts := range allTS {
		tskey := ts.Spec.HostGroup + "_hg"
		if tskey == key {
			return true
		}
	}
	allVS := ctlr.getAllVSFromMonitoredNamespaces()
	for _, vs := range allVS {
		vskey := vs.Spec.HostGroup + "_hg"
		if vskey == key {
			return true
		}
	}
	return false
}

func (ctlr *Controller) RemoveIPAMCRHostSpec(ipamCR *ficV1.IPAM, key string, index int) (res *ficV1.IPAM, err error) {
	isExists := false
	if strings.HasSuffix(key, "_hg") {
		isExists = ctlr.VerifyIPAMAssociatedHostGroupExists(key)
	}
	if !isExists {
		delete(ctlr.resources.ipamContext, key)
		ipamCR.Spec.HostSpecs = append(ipamCR.Spec.HostSpecs[:index], ipamCR.Spec.HostSpecs[index+1:]...)
		ipamCR.SetResourceVersion(ipamCR.ResourceVersion)
		return ctlr.ipamCli.Update(ipamCR)
	}
	return res, err
}

func (ctlr *Controller) releaseIP(ipamLabel string, host string, key string) string {
	ipamCR := ctlr.getIPAMCR()
	var ip string
	if ipamCR == nil || ipamLabel == "" {
		return ip
	}
	index := -1
	if host != "" {
		//Find index for deleted host
		for i, hostSpec := range ipamCR.Spec.HostSpecs {
			if hostSpec.IPAMLabel == ipamLabel && hostSpec.Host == host {
				index = i
				break
			}
		}
		//Find IP address for deleted host
		for _, ipst := range ipamCR.Status.IPStatus {
			if ipst.IPAMLabel == ipamLabel && ipst.Host == host {
				ip = ipst.IP
			}
		}
		if index != -1 {
			_, err := ctlr.RemoveIPAMCRHostSpec(ipamCR, key, index)
			if err != nil {
				log.Errorf("[ipam] ipam hostspec update error: %v", err)
				return ""
			}
			log.Debug("[ipam] Updated IPAM CR hostspec while releasing IP.")
		}
	} else if key != "" {
		//Find index for deleted key
		for i, hostSpec := range ipamCR.Spec.HostSpecs {
			if hostSpec.IPAMLabel == ipamLabel && hostSpec.Key == key {
				index = i
				break
			}
		}
		//Find IP address for deleted host
		for _, ipst := range ipamCR.Status.IPStatus {
			if ipst.IPAMLabel == ipamLabel && ipst.Key == key {
				ip = ipst.IP
				break
			}
		}
		if index != -1 {
			_, err := ctlr.RemoveIPAMCRHostSpec(ipamCR, key, index)
			if err != nil {
				log.Errorf("[ipam] ipam hostspec update error: %v", err)
				return ""
			}
			log.Debug("[ipam] Updated IPAM CR hostspec while releasing IP.")
		}

	} else {
		log.Debugf("[IPAM] Invalid host and key.")
	}

	if len(ctlr.resources.ipamContext) == 0 {
		ctlr.ipamHostSpecEmpty = true
	}

	return ip
}

// updatePoolMembersForNodePort updates the pool with pool members for a
// service created in nodeport mode.
func (ctlr *Controller) updatePoolMembersForNodePort(
	rsCfg *ResourceConfig,
	namespace string,
) {
	_, ok1 := ctlr.getNamespacedCRInformer(namespace)
	_, ok2 := ctlr.getNamespacedCommonInformer(namespace)
	if !ok1 && !ok2 {
		log.Errorf("Informer not found for namespace: %v", namespace)
		return
	}

	for index, pool := range rsCfg.Pools {
		svcName := pool.ServiceName
		svcKey := pool.ServiceNamespace + "/" + svcName

		poolMemInfo, ok := ctlr.resources.poolMemCache[svcKey]
		if (!ok || len(poolMemInfo.memberMap) == 0) && pool.ServiceNamespace == namespace {
			rsCfg.Pools[index].Members = []PoolMember{}
			continue
		}

		if !(poolMemInfo.svcType == v1.ServiceTypeNodePort ||
			poolMemInfo.svcType == v1.ServiceTypeLoadBalancer) {
			log.Debugf("Requested service backend %s not of NodePort or LoadBalancer type",
				svcKey)
		}

		for _, svcPort := range poolMemInfo.portSpec {
			if svcPort.TargetPort == pool.ServicePort {
				rsCfg.MetaData.Active = true
				rsCfg.Pools[index].Members =
					ctlr.getEndpointsForNodePort(svcPort.NodePort, pool.NodeMemberLabel)
			}
		}
		//check if endpoints are found
		if rsCfg.Pools[index].Members == nil {
			log.Errorf("[CORE]Endpoints could not be fetched for service %v with targetPort %v", svcName, pool.ServicePort.IntVal)
		}
	}
}

// updatePoolMembersForCluster updates the pool with pool members for a
// service created in cluster mode.
func (ctlr *Controller) updatePoolMembersForCluster(
	rsCfg *ResourceConfig,
	namespace string,
) {
	for index, pool := range rsCfg.Pools {
		svcName := pool.ServiceName
		svcKey := pool.ServiceNamespace + "/" + svcName

		poolMemInfo, ok := ctlr.resources.poolMemCache[svcKey]

		if (!ok || len(poolMemInfo.memberMap) == 0) && pool.ServiceNamespace == namespace {
			rsCfg.Pools[index].Members = []PoolMember{}
			continue
		}

		for ref, mems := range poolMemInfo.memberMap {
			if ref.name != pool.ServicePort.StrVal && ref.port != pool.ServicePort.IntVal {
				continue
			}
			rsCfg.MetaData.Active = true
			rsCfg.Pools[index].Members = mems
		}
		//check if endpoints are found
		if rsCfg.Pools[index].Members == nil {
			log.Errorf("[CORE]Endpoints could not be fetched for service %v with targetPort %v", svcName, pool.ServicePort.IntVal)
		}
	}
}

// updatePoolMembersForNodePortLocal updates the pool with pool members for a
// service created in clusterIP and annotated with nodeportlocal.antrea.io/enabled
func (ctlr *Controller) updatePoolMembersForNPL(
	rsCfg *ResourceConfig,
	namespace string,
) {
	_, ok := ctlr.getNamespacedCRInformer(namespace)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", namespace)
		return
	}

	for index, pool := range rsCfg.Pools {
		svcName := pool.ServiceName
		svcKey := pool.ServiceNamespace + "/" + svcName
		poolMemInfo := ctlr.resources.poolMemCache[svcKey]
		if poolMemInfo.svcType == v1.ServiceTypeNodePort {
			log.Debugf("Requested service backend %s is of type NodePort is not valid for nodeportlocal mode.",
				svcKey)
			return
		}
		pods := ctlr.GetPodsForService(namespace, svcName, true)
		if pods != nil {
			for _, svcPort := range poolMemInfo.portSpec {
				if svcPort.TargetPort == pool.ServicePort {
					podPort := svcPort.TargetPort
					rsCfg.MetaData.Active = true
					rsCfg.Pools[index].Members =
						ctlr.getEndpointsForNPL(podPort, pods)

				}
			}
		}
	}
}

// getEndpointsForNodePort returns members.
func (ctlr *Controller) getEndpointsForNodePort(
	nodePort int32,
	nodeMemberLabel string,
) []PoolMember {
	var nodes []Node
	if nodeMemberLabel == "" {
		nodes = ctlr.getNodesFromCache()
	} else {
		nodes = ctlr.getNodesWithLabel(nodeMemberLabel)
	}
	var members []PoolMember
	for _, v := range nodes {
		member := PoolMember{
			Address: v.Addr,
			Port:    nodePort,
			Session: "user-enabled",
		}
		members = append(members, member)
	}

	return members
}

// getEndpointsForNPL returns members.
func (ctlr *Controller) getEndpointsForNPL(
	targetPort intstr.IntOrString,
	pods []*v1.Pod,
) []PoolMember {
	var members []PoolMember
	for _, pod := range pods {
		anns, found := ctlr.resources.nplStore[pod.Namespace+"/"+pod.Name]
		if !found {
			continue
		}
		var podPort int32
		//Support for named targetPort
		if targetPort.StrVal != "" {
			targetPortStr := targetPort.StrVal
			//Get the containerPort matching targetPort from pod spec.
			for _, container := range pod.Spec.Containers {
				for _, port := range container.Ports {
					portStr := port.Name
					if targetPortStr == portStr {
						podPort = port.ContainerPort
					}
				}
			}
		} else {
			// targetPort with int value
			podPort = targetPort.IntVal
		}
		for _, annotation := range anns {
			if annotation.PodPort == podPort {
				member := PoolMember{
					Address: annotation.NodeIP,
					Port:    annotation.NodePort,
					Session: "user-enabled",
				}
				members = append(members, member)
			}
		}
	}
	return members
}

// containsNode returns true for a valid node.
func containsNode(nodes []Node, name string) bool {
	for _, node := range nodes {
		if node.Name == name {
			return true
		}
	}
	return false
}

// processTransportServers takes the Transport Server as input and processes all
// associated TransportServers to create a resource config(Internal DataStructure)
// or to update if exists already.
func (ctlr *Controller) processTransportServers(
	virtual *cisapiv1.TransportServer,
	isTSDeleted bool,
) error {
	startTime := time.Now()
	defer func() {
		endTime := time.Now()
		log.Debugf("Finished syncing transport servers %+v (%v)",
			virtual, endTime.Sub(startTime))
	}()

	// Skip validation for a deleted Virtual Server
	if !isTSDeleted {
		// check if the virutal server matches all the requirements.
		vkey := virtual.ObjectMeta.Namespace + "/" + virtual.ObjectMeta.Name
		valid := ctlr.checkValidTransportServer(virtual)
		if false == valid {
			log.Errorf("TransportServer %s, is not valid",
				vkey)
			return nil
		}
	}
	ctlr.TeemData.Lock()
	ctlr.TeemData.ResourceType.TransportServer[virtual.ObjectMeta.Namespace] = len(ctlr.getAllTransportServers(virtual.Namespace))
	ctlr.TeemData.Unlock()

	if isTSDeleted {
		ctlr.TeemData.Lock()
		ctlr.TeemData.ResourceType.TransportServer[virtual.ObjectMeta.Namespace]--
		ctlr.TeemData.Unlock()
	}

	var ip string
	var key string
	var status int
	key = virtual.ObjectMeta.Namespace + "/" + virtual.ObjectMeta.Name + "_ts"
	if ctlr.ipamCli != nil {
		if virtual.Spec.HostGroup != "" {
			key = virtual.Spec.HostGroup + "_hg"
		}
		if isTSDeleted && virtual.Spec.VirtualServerAddress == "" {
			ip = ctlr.releaseIP(virtual.Spec.IPAMLabel, "", key)
		} else if virtual.Spec.VirtualServerAddress != "" {
			ip = virtual.Spec.VirtualServerAddress
		} else {
			ip, status = ctlr.requestIP(virtual.Spec.IPAMLabel, "", key)

			switch status {
			case NotEnabled:
				log.Debug("IPAM Custom Resource Not Available")
				return nil
			case InvalidInput:
				log.Debugf("IPAM Invalid IPAM Label: %v for Transport Server: %s/%s",
					virtual.Spec.IPAMLabel, virtual.Namespace, virtual.Name)
				return nil
			case NotRequested:
				return fmt.Errorf("unable to make IPAM Request, will be re-requested soon")
			case Requested:
				log.Debugf("IP address requested for Transport Server: %s/%s", virtual.Namespace, virtual.Name)
				return nil
			}
			virtual.Status.VSAddress = ip
		}
	} else {
		if virtual.Spec.VirtualServerAddress == "" {
			return fmt.Errorf("No VirtualServer address in TS or IPAM found.")
		}
		ip = virtual.Spec.VirtualServerAddress
	}

	var rsName string
	if virtual.Spec.VirtualServerName != "" {
		rsName = formatCustomVirtualServerName(
			virtual.Spec.VirtualServerName,
			virtual.Spec.VirtualServerPort,
		)
	} else {
		rsName = formatVirtualServerName(
			ip,
			virtual.Spec.VirtualServerPort,
		)
	}

	if isTSDeleted {
		rsMap := ctlr.resources.getPartitionResourceMap(ctlr.Partition)
		ctlr.deleteSvcDepResource(rsName, rsMap[rsName])
		ctlr.deleteVirtualServer(ctlr.Partition, rsName)
		return nil
	}

	rsCfg := &ResourceConfig{}
	rsCfg.Virtual.Partition = ctlr.Partition
	rsCfg.MetaData.ResourceType = TransportServer
	rsCfg.Virtual.Enabled = true
	rsCfg.Virtual.Name = rsName
	rsCfg.MetaData.hosts = append(rsCfg.MetaData.hosts, virtual.Spec.Host)
	rsCfg.Virtual.IpProtocol = virtual.Spec.Type
	rsCfg.MetaData.namespace = virtual.ObjectMeta.Namespace
	rsCfg.MetaData.baseResources = make(map[string]string)
	rsCfg.Virtual.SetVirtualAddress(
		ip,
		virtual.Spec.VirtualServerPort,
	)
	plc, err := ctlr.getPolicyFromTransportServer(virtual)
	if plc != nil {
		err := ctlr.handleTSResourceConfigForPolicy(rsCfg, plc)
		if err != nil {
			log.Errorf("%v", err)
			return nil
		}
	}
	if err != nil {
		log.Errorf("%v", err)
		return nil
	}

	log.Debugf("Processing Transport Server %s for port %v",
		virtual.ObjectMeta.Name, virtual.Spec.VirtualServerPort)
	rsCfg.MetaData.baseResources[virtual.ObjectMeta.Namespace+"/"+virtual.ObjectMeta.Name] = TransportServer
	err = ctlr.prepareRSConfigFromTransportServer(
		rsCfg,
		virtual,
	)
	if err != nil {
		log.Errorf("Cannot Publish TransportServer %s", virtual.ObjectMeta.Name)
		return nil
	}

	ctlr.updateSvcDepResources(rsName, rsCfg)

	if ctlr.PoolMemberType == NodePort {
		ctlr.updatePoolMembersForNodePort(rsCfg, virtual.ObjectMeta.Namespace)
	} else {
		ctlr.updatePoolMembersForCluster(rsCfg, virtual.ObjectMeta.Namespace)
	}

	rsMap := ctlr.resources.getPartitionResourceMap(ctlr.Partition)
	rsMap[rsName] = rsCfg

	return nil
}

// getAllTSFromMonitoredNamespaces returns list of all valid TransportServers in monitored namespaces.
func (ctlr *Controller) getAllTSFromMonitoredNamespaces() []*cisapiv1.TransportServer {
	var allVirtuals []*cisapiv1.TransportServer
	if ctlr.watchingAllNamespaces() {
		return ctlr.getAllTransportServers("")
	}
	for ns := range ctlr.namespaces {
		allVirtuals = append(allVirtuals, ctlr.getAllTransportServers(ns)...)
	}
	return allVirtuals
}

// getAllTransportServers returns list of all valid TransportServers in rkey namespace.
func (ctlr *Controller) getAllTransportServers(namespace string) []*cisapiv1.TransportServer {
	var allVirtuals []*cisapiv1.TransportServer

	crInf, ok := ctlr.getNamespacedCRInformer(namespace)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", namespace)
		return nil
	}
	var orderedTSs []interface{}
	var err error

	if namespace == "" {
		orderedTSs = crInf.tsInformer.GetIndexer().List()
	} else {
		orderedTSs, err = crInf.tsInformer.GetIndexer().ByIndex("namespace", namespace)
		if err != nil {
			log.Errorf("Unable to get list of TransportServers for namespace '%v': %v",
				namespace, err)
			return nil
		}
	}
	for _, obj := range orderedTSs {
		vs := obj.(*cisapiv1.TransportServer)
		// TODO Validate the TransportServers List to check if all the vs are valid.
		allVirtuals = append(allVirtuals, vs)
	}

	return allVirtuals
}

// getAllLBServices returns list of all valid LB Services in rkey namespace.
func (ctlr *Controller) getAllLBServices(namespace string) []*v1.Service {
	var allLBServices []*v1.Service

	comInf, ok := ctlr.getNamespacedCommonInformer(namespace)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", namespace)
		return nil
	}
	var orderedSVCs []interface{}
	var err error

	if namespace == "" {
		orderedSVCs = comInf.svcInformer.GetIndexer().List()
	} else {
		orderedSVCs, err = comInf.svcInformer.GetIndexer().ByIndex("namespace", namespace)
		if err != nil {
			log.Errorf("Unable to get list of Services for namespace '%v': %v",
				namespace, err)
			return nil
		}
	}
	for _, obj := range orderedSVCs {
		svc := obj.(*v1.Service)
		if svc.Spec.Type == v1.ServiceTypeLoadBalancer {
			allLBServices = append(allLBServices, svc)
		}
	}

	return allLBServices
}

// getTransportServersForService gets the List of VirtualServers which are effected
// by the addition/deletion/updation of service.
func (ctlr *Controller) getTransportServersForService(svc *v1.Service) []*cisapiv1.TransportServer {

	allVirtuals := ctlr.getAllTransportServers(svc.ObjectMeta.Namespace)
	if nil == allVirtuals {
		log.Infof("No VirtualServers for TransportServer found in namespace %s",
			svc.ObjectMeta.Namespace)
		return nil
	}

	// find VirtualServers that reference the service
	virtualsForService := filterTransportServersForService(allVirtuals, svc)
	if nil == virtualsForService {
		log.Debugf("Change in Service %s does not effect any VirtualServer for TransportServer",
			svc.ObjectMeta.Name)
		return nil
	}
	return virtualsForService
}

// filterTransportServersForService returns list of VirtualServers that are
// affected by the service under process.
func filterTransportServersForService(allVirtuals []*cisapiv1.TransportServer,
	svc *v1.Service) []*cisapiv1.TransportServer {

	var result []*cisapiv1.TransportServer
	svcName := svc.ObjectMeta.Name
	svcNamespace := svc.ObjectMeta.Namespace

	for _, vs := range allVirtuals {
		if vs.ObjectMeta.Namespace != svcNamespace {
			continue
		}

		isValidVirtual := false
		if vs.Spec.Pool.Service == svcName {
			isValidVirtual = true
		}
		if !isValidVirtual {
			continue
		}
		result = append(result, vs)
	}

	return result
}

//func (ctlr *Controller) getAllServicesFromMonitoredNamespaces() []*v1.Service {
//	var svcList []*v1.Service
//	if ctlr.watchingAllNamespaces() {
//		objList := ctlr.comInformers[""].svcInformer.GetIndexer().List()
//		for _, obj := range objList {
//			svcList = append(svcList, obj.(*v1.Service))
//		}
//		return svcList
//	}
//
//	for ns := range ctlr.namespaces {
//		objList := ctlr.comInformers[ns].svcInformer.GetIndexer().List()
//		for _, obj := range objList {
//			svcList = append(svcList, obj.(*v1.Service))
//		}
//	}
//
//	return svcList
//}

//// Get List of VirtualServers associated with the IPAM resource
//func (ctlr *Controller) getVirtualServersForIPAM(ipam *ficV1.IPAM) []*cisapiv1.VirtualServer {
//	log.Debug("[ipam] Syncing IPAM dependent virtual servers")
//	var allVS, vss []*cisapiv1.VirtualServer
//	allVS = ctlr.getAllVSFromMonitoredNamespaces()
//	for _, status := range ipam.Status.IPStatus {
//		for _, vs := range allVS {
//			key := vs.ObjectMeta.Namespace + "/" + vs.Spec.HostGroup
//			if status.Host == vs.Spec.Host || status.Key == key {
//				vss = append(vss, vs)
//				break
//			}
//		}
//	}
//	return vss
//}

//// Get List of TransportServers associated with the IPAM resource
//func (ctlr *Controller) getTransportServersForIPAM(ipam *ficV1.IPAM) []*cisapiv1.TransportServer {
//	log.Debug("[ipam] Syncing IPAM dependent transport servers")
//	var allTS, tss []*cisapiv1.TransportServer
//	allTS = ctlr.getAllTSFromMonitoredNamespaces()
//	for _, status := range ipam.Status.IPStatus {
//		for _, ts := range allTS {
//			key := ts.ObjectMeta.Namespace + "/" + ts.ObjectMeta.Name + "_ts"
//			if status.Key == key {
//				tss = append(tss, ts)
//				break
//			}
//		}
//	}
//	return tss
//}

//// Get List of ingLink associated with the IPAM resource
//func (ctlr *Controller) getIngressLinkForIPAM(ipam *ficV1.IPAM) []*cisapiv1.IngressLink {
//	var allIngLinks, ils []*cisapiv1.IngressLink
//	allIngLinks = ctlr.getAllIngLinkFromMonitoredNamespaces()
//	if allIngLinks == nil {
//		return nil
//	}
//	for _, status := range ipam.Status.IPStatus {
//		for _, il := range allIngLinks {
//			key := il.ObjectMeta.Namespace + "/" + il.ObjectMeta.Name + "_il"
//			if status.Key == key {
//				ils = append(ils, il)
//				break
//			}
//		}
//	}
//	return ils
//}

//func (ctlr *Controller) syncAndGetServicesForIPAM(ipam *ficV1.IPAM) []*v1.Service {
//
//	allServices := ctlr.getAllServicesFromMonitoredNamespaces()
//	if allServices == nil {
//		return nil
//	}
//	var svcList []*v1.Service
//	var staleSpec []*ficV1.IPSpec
//	for _, IPSpec := range ipam.Status.IPStatus {
//		for _, svc := range allServices {
//			svcKey := svc.Namespace + "/" + svc.Name + "_svc"
//			if IPSpec.Key == svcKey {
//				if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
//					staleSpec = append(staleSpec, IPSpec)
//					ctlr.eraseLBServiceIngressStatus(svc)
//				} else {
//					svcList = append(svcList, svc)
//				}
//			}
//		}
//	}
//
//	for _, IPSpec := range staleSpec {
//		ctlr.releaseIP(IPSpec.IPAMLabel, "", IPSpec.Key)
//	}
//
//	return svcList
//}

func (ctlr *Controller) processLBServices(
	svc *v1.Service,
	isSVCDeleted bool,
) error {
	if ctlr.ipamCli == nil {
		log.Error("IPAM is not enabled, Unable to process Services of Type LoadBalancer")
		return nil
	}

	ipamLabel, ok := svc.Annotations[LBServiceIPAMLabelAnnotation]
	if !ok {
		log.Errorf("Not found %v in %v/%v. Unable to process.",
			LBServiceIPAMLabelAnnotation,
			svc.Namespace,
			svc.Name,
		)
		return nil
	}

	svcKey := svc.Namespace + "/" + svc.Name + "_svc"
	var ip string
	var status int
	if isSVCDeleted {
		ip = ctlr.releaseIP(ipamLabel, "", svcKey)
	} else {
		ip, status = ctlr.requestIP(ipamLabel, "", svcKey)

		switch status {
		case NotEnabled:
			log.Debug("IPAM Custom Resource Not Available")
			return nil
		case InvalidInput:
			log.Debugf("IPAM Invalid IPAM Label: %v for service: %s/%s", ipamLabel, svc.Namespace, svc.Name)
			return nil
		case NotRequested:
			return fmt.Errorf("unable to make IPAM Request, will be re-requested soon")
		case Requested:
			log.Debugf("IP address requested for service: %s/%s", svc.Namespace, svc.Name)
			return nil
		}
	}

	if !isSVCDeleted {
		ctlr.setLBServiceIngressStatus(svc, ip)
	} else {
		ctlr.unSetLBServiceIngressStatus(svc, ip)
	}

	for _, portSpec := range svc.Spec.Ports {

		log.Debugf("Processing Service Type LB %s for port %v",
			svc.ObjectMeta.Name, portSpec)

		rsName := AS3NameFormatter(fmt.Sprintf("vs_lb_svc_%s_%s_%s_%v", svc.Namespace, svc.Name, ip, portSpec.Port))
		if isSVCDeleted {
			rsMap := ctlr.resources.getPartitionResourceMap(ctlr.Partition)
			ctlr.deleteSvcDepResource(rsName, rsMap[rsName])
			ctlr.deleteVirtualServer(ctlr.Partition, rsName)
			continue
		}

		rsCfg := &ResourceConfig{}
		rsCfg.Virtual.Partition = ctlr.Partition
		rsCfg.Virtual.IpProtocol = strings.ToLower(string(portSpec.Protocol))
		rsCfg.MetaData.ResourceType = TransportServer
		rsCfg.MetaData.namespace = svc.ObjectMeta.Namespace
		rsCfg.Virtual.Enabled = true
		rsCfg.Virtual.Name = rsName
		rsCfg.Virtual.SetVirtualAddress(
			ip,
			portSpec.Port,
		)
		processingError := false
		// Handle policy
		plc, err := ctlr.getPolicyFromLBService(svc)
		if plc != nil {
			err := ctlr.handleTSResourceConfigForPolicy(rsCfg, plc)
			if err != nil {
				log.Errorf("%v", err)
				processingError = true
			}
		}
		if err != nil {
			processingError = true
			log.Errorf("%v", err)
		}

		if processingError {
			log.Errorf("Cannot Publish LB Service %s", svc.ObjectMeta.Name)
			break
		}

		_ = ctlr.prepareRSConfigFromLBService(rsCfg, svc, portSpec)

		ctlr.updateSvcDepResources(rsName, rsCfg)

		if ctlr.PoolMemberType == NodePort {
			ctlr.updatePoolMembersForNodePort(rsCfg, svc.Namespace)
		} else if ctlr.PoolMemberType == NodePortLocal {
			//supported with antrea cni.
			ctlr.updatePoolMembersForNPL(rsCfg, svc.Namespace)
		} else {
			ctlr.updatePoolMembersForCluster(rsCfg, svc.Namespace)
		}

		rsMap := ctlr.resources.getPartitionResourceMap(ctlr.Partition)

		rsMap[rsName] = rsCfg
	}

	return nil
}

func (ctlr *Controller) processService(
	svc *v1.Service,
	eps *v1.Endpoints,
	isSVCDeleted bool,
) error {
	namespace := svc.Namespace
	svcKey := svc.Namespace + "/" + svc.Name
	if isSVCDeleted {
		delete(ctlr.resources.poolMemCache, svcKey)
		return nil
	}

	if eps == nil {
		comInf, ok := ctlr.getNamespacedCommonInformer(namespace)
		if !ok {
			log.Errorf("Informer not found for namespace: %v", namespace)
			return fmt.Errorf("unable to process Service: %v", svcKey)
		}
		epInf := comInf.epsInformer
		item, found, _ := epInf.GetIndexer().GetByKey(svcKey)
		if !found {
			return fmt.Errorf("Endpoints for service '%v' not found!", svcKey)
		}
		eps, _ = item.(*v1.Endpoints)
	}

	pmi := poolMembersInfo{
		svcType:   svc.Spec.Type,
		portSpec:  svc.Spec.Ports,
		memberMap: make(map[portRef][]PoolMember),
	}

	nodes := ctlr.getNodesFromCache()
	for _, subset := range eps.Subsets {
		for _, p := range subset.Ports {
			var members []PoolMember
			for _, addr := range subset.Addresses {
				// Checking for headless services
				if svc.Spec.ClusterIP == "None" || (addr.NodeName != nil && containsNode(nodes, *addr.NodeName)) {
					member := PoolMember{
						Address: addr.IP,
						Port:    p.Port,
						Session: "user-enabled",
					}
					members = append(members, member)
				}
			}
			portKey := portRef{name: p.Name, port: p.Port}
			pmi.memberMap[portKey] = members
		}
	}

	ctlr.resources.poolMemCache[svcKey] = pmi

	return nil
}

func (ctlr *Controller) processExternalDNS(edns *cisapiv1.ExternalDNS, isDelete bool) {

	if gtmPartitionConfig, ok := ctlr.resources.gtmConfig[DEFAULT_PARTITION]; ok {
		if processedWIP, ok := gtmPartitionConfig.WideIPs[edns.Spec.DomainName]; ok {
			if processedWIP.UID != string(edns.UID) {
				log.Errorf("EDNS with same domain name %s present", edns.Spec.DomainName)
				return
			}
		}
	}

	if isDelete {
		if _, ok := ctlr.resources.gtmConfig[DEFAULT_PARTITION]; !ok {
			return
		}

		delete(ctlr.resources.gtmConfig[DEFAULT_PARTITION].WideIPs, edns.Spec.DomainName)
		ctlr.TeemData.Lock()
		ctlr.TeemData.ResourceType.ExternalDNS[edns.Namespace]--
		ctlr.TeemData.Unlock()
		return
	}

	ctlr.TeemData.Lock()
	ctlr.TeemData.ResourceType.ExternalDNS[edns.Namespace] = len(ctlr.getAllExternalDNS(edns.Namespace))
	ctlr.TeemData.Unlock()

	wip := WideIP{
		DomainName: edns.Spec.DomainName,
		RecordType: edns.Spec.DNSRecordType,
		LBMethod:   edns.Spec.LoadBalanceMethod,
		UID:        string(edns.UID),
	}

	if edns.Spec.DNSRecordType == "" {
		wip.RecordType = "A"
	}
	if edns.Spec.LoadBalanceMethod == "" {
		wip.LBMethod = "round-robin"
	}

	log.Debugf("Processing WideIP: %v", edns.Spec.DomainName)

	var partitions []string
	switch ctlr.mode {
	case OpenShiftMode:
		partitions = ctlr.resources.GetLTMPartitions()
	default:
		partitions = append(partitions, DEFAULT_PARTITION)
	}

	for _, pl := range edns.Spec.Pools {
		UniquePoolName := edns.Spec.DomainName + "_" + AS3NameFormatter(strings.TrimPrefix(ctlr.Agent.BIGIPURL, "https://")) + "_" + ctlr.Partition
		log.Debugf("Processing WideIP Pool: %v", UniquePoolName)
		pool := GSLBPool{
			Name:          UniquePoolName,
			RecordType:    pl.DNSRecordType,
			LBMethod:      pl.LoadBalanceMethod,
			PriorityOrder: pl.PriorityOrder,
			DataServer:    pl.DataServerName,
		}

		if pl.DNSRecordType == "" {
			pool.RecordType = "A"
		}
		if pl.LoadBalanceMethod == "" {
			pool.LBMethod = "round-robin"
		}
		for _, partition := range partitions {
			rsMap := ctlr.resources.getPartitionResourceMap(partition)

			for vsName, vs := range rsMap {
				var found bool
				for _, host := range vs.MetaData.hosts {
					if host == edns.Spec.DomainName {
						found = true
						break
					}
				}
				if found {
					//No need to add insecure VS into wideIP pool if VS configured with httpTraffic as redirect
					if vs.MetaData.Protocol == "http" && (vs.MetaData.httpTraffic == TLSRedirectInsecure || vs.MetaData.httpTraffic == TLSAllowInsecure) {
						continue
					}
					preGTMServerName := ""
					if ctlr.Agent.ccclGTMAgent {
						preGTMServerName = fmt.Sprintf("%v:", pl.DataServerName)
					}
					// add only one VS member to pool.
					if len(pool.Members) > 0 && strings.HasPrefix(vsName, "ingress_link_") {
						if strings.HasSuffix(vsName, "_443") {
							pool.Members[0] = fmt.Sprintf("%v/%v/Shared/%v", preGTMServerName, partition, vsName)
							if partition != ctlr.Partition {
								// Modify pool name to partition containing VS
								pool.Name = edns.Spec.DomainName + "_" + AS3NameFormatter(strings.TrimPrefix(ctlr.Agent.BIGIPURL, "https://")) + "_" + partition
							}
						}
						continue
					}
					log.Debugf("Adding WideIP Pool Member: %v", fmt.Sprintf("/%v/Shared/%v",
						partition, vsName))
					// Modify pool name to partition containing VS
					if partition != ctlr.Partition {
						// Modify pool name to partition containing VS
						pool.Name = edns.Spec.DomainName + "_" + AS3NameFormatter(strings.TrimPrefix(ctlr.Agent.BIGIPURL, "https://")) + "_" + partition
					}
					pool.Members = append(
						pool.Members,
						fmt.Sprintf("%v/%v/Shared/%v", preGTMServerName, partition, vsName),
					)
				}
			}
		}
		if len(pl.Monitors) > 0 {
			var monitors []Monitor
			for i, monitor := range pl.Monitors {
				monitors = append(monitors,
					Monitor{
						Name:      fmt.Sprintf("%s_monitor%d", UniquePoolName, i),
						Partition: "Common",
						Type:      monitor.Type,
						Interval:  monitor.Interval,
						Send:      monitor.Send,
						Recv:      monitor.Recv,
						Timeout:   monitor.Timeout})
			}
			pool.Monitors = monitors

		} else if pl.Monitor.Type != "" {
			// TODO: Need to change to DEFAULT_PARTITION from Common, once Agent starts to support DEFAULT_PARTITION
			var monitors []Monitor

			if pl.Monitor.Type == "http" || pl.Monitor.Type == "https" {
				monitors = append(monitors,
					Monitor{
						Name:      UniquePoolName + "_monitor",
						Partition: "Common",
						Type:      pl.Monitor.Type,
						Interval:  pl.Monitor.Interval,
						Send:      pl.Monitor.Send,
						Recv:      pl.Monitor.Recv,
						Timeout:   pl.Monitor.Timeout,
					})
			} else {
				monitors = append(monitors,
					Monitor{
						Name:      UniquePoolName + "_monitor",
						Partition: "Common",
						Type:      pl.Monitor.Type,
						Interval:  pl.Monitor.Interval,
						Timeout:   pl.Monitor.Timeout,
					})
			}
			pool.Monitors = monitors
		}
		wip.Pools = append(wip.Pools, pool)
	}
	if _, ok := ctlr.resources.gtmConfig[DEFAULT_PARTITION]; !ok {
		ctlr.resources.gtmConfig[DEFAULT_PARTITION] = GTMPartitionConfig{
			WideIPs: make(map[string]WideIP),
		}
	}

	ctlr.resources.gtmConfig[DEFAULT_PARTITION].WideIPs[wip.DomainName] = wip
	return
}

func (ctlr *Controller) getAllExternalDNS(namespace string) []*cisapiv1.ExternalDNS {
	var allEDNS []*cisapiv1.ExternalDNS
	comInf, ok := ctlr.getNamespacedCommonInformer(namespace)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", namespace)
		return nil
	}
	var orderedEDNSs []interface{}
	var err error

	if namespace == "" {
		orderedEDNSs = comInf.ednsInformer.GetIndexer().List()
	} else {
		orderedEDNSs, err = comInf.ednsInformer.GetIndexer().ByIndex("namespace", namespace)
		if err != nil {
			log.Errorf("Unable to get list of ExternalDNSs for namespace '%v': %v",
				namespace, err)
			return allEDNS
		}
	}

	for _, obj := range orderedEDNSs {
		edns := obj.(*cisapiv1.ExternalDNS)
		allEDNS = append(allEDNS, edns)
	}

	return allEDNS
}

func (ctlr *Controller) ProcessRouteEDNS(hosts []string) {
	if len(ctlr.processedHostPath.removedHosts) > 0 {
		removedHosts := ctlr.processedHostPath.removedHosts
		ctlr.processedHostPath.Lock()
		ctlr.processedHostPath.removedHosts = make([]string, 0)
		ctlr.processedHostPath.Unlock()
		//This will remove existing EDNS pool members
		ctlr.ProcessAssociatedExternalDNS(removedHosts)
	}
	if len(hosts) > 0 {
		ctlr.ProcessAssociatedExternalDNS(hosts)
	}
}

func (ctlr *Controller) ProcessAssociatedExternalDNS(hostnames []string) {
	var allEDNS []*cisapiv1.ExternalDNS
	if ctlr.watchingAllNamespaces() {
		allEDNS = ctlr.getAllExternalDNS("")
	} else {
		for ns := range ctlr.namespaces {
			allEDNS = append(allEDNS, ctlr.getAllExternalDNS(ns)...)
		}
	}
	for _, edns := range allEDNS {
		for _, hostname := range hostnames {
			if edns.Spec.DomainName == hostname {
				ctlr.processExternalDNS(edns, false)
			}
		}
	}
}

// Validate certificate hostname
func checkCertificateHost(host string, certificate []byte, key []byte) bool {
	cert, certErr := tls.X509KeyPair(certificate, key)
	if certErr != nil {
		log.Errorf("Failed to validate TLS cert and key: %v", certErr)
		return false
	}
	x509cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		log.Errorf("failed to parse certificate; %s", err)
		return false
	}
	ok := x509cert.VerifyHostname(host)
	if ok != nil {
		log.Debugf("Error: Hostname in virtualserver does not match with certificate hostname: %v", ok)
		return false
	}
	return true
}

func (ctlr *Controller) processIPAM(ipam *ficV1.IPAM) error {
	var keysToProcess []string

	if ctlr.ipamHostSpecEmpty {
		ipamRes, _ := ctlr.ipamCli.Get(ipam.Namespace, ipam.Name)
		if len(ipamRes.Spec.HostSpecs) > 0 {
			ctlr.ipamHostSpecEmpty = false
		}
	}

	if !ctlr.ipamHostSpecEmpty {
		for _, ipSpec := range ipam.Status.IPStatus {
			if cachedIPSpec, ok := ctlr.resources.ipamContext[ipSpec.Key]; ok {
				if cachedIPSpec.IP != ipSpec.IP {
					// TODO: Delete the VS with old IP in BIGIP in case of FIC reboot
					keysToProcess = append(keysToProcess, ipSpec.Key)
				}
			} else {
				ctlr.resources.ipamContext[ipSpec.Key] = *ipSpec
				keysToProcess = append(keysToProcess, ipSpec.Key)
			}
		}
	}

	for _, pKey := range keysToProcess {
		idx := strings.LastIndex(pKey, "_")
		if idx == -1 {
			continue
		}
		rscKind := pKey[idx+1:]
		var crInf *CRInformer
		var comInf *CommonInformer
		var ns string
		if rscKind != "hg" {
			splits := strings.Split(pKey, "/")
			ns = splits[0]
			var ok bool
			crInf, ok = ctlr.getNamespacedCRInformer(ns)
			comInf, ok = ctlr.getNamespacedCommonInformer(ns)
			if !ok {
				log.Errorf("Informer not found for namespace: %v", ns)
				return nil
			}
		}
		switch rscKind {
		case "hg":
			// For Virtual Server
			var vss []*cisapiv1.VirtualServer
			vss = ctlr.getAllVSFromMonitoredNamespaces()
			for _, vs := range vss {
				key := vs.Spec.HostGroup + "_hg"
				if pKey == key {
					ctlr.TeemData.Lock()
					ctlr.TeemData.ResourceType.IPAMVS[ns]++
					ctlr.TeemData.Unlock()
					err := ctlr.processVirtualServers(vs, false)
					if err != nil {
						log.Errorf("Unable to process IPAM entry: %v", pKey)
					}
					break
				}
			}
			// For Transport Server
			var tss []*cisapiv1.TransportServer
			tss = ctlr.getAllTSFromMonitoredNamespaces()
			for _, ts := range tss {
				key := ts.Spec.HostGroup + "_hg"
				if pKey == key {
					ctlr.TeemData.Lock()
					ctlr.TeemData.ResourceType.IPAMTS[ns]++
					ctlr.TeemData.Unlock()
					err := ctlr.processTransportServers(ts, false)
					if err != nil {
						log.Errorf("Unable to process IPAM entry: %v", pKey)
					}
					break
				}
			}
		case "host":
			var vss []*cisapiv1.VirtualServer
			vss = ctlr.getAllVirtualServers(ns)
			for _, vs := range vss {
				key := vs.Namespace + "/" + vs.Spec.Host + "_host"
				if pKey == key {
					ctlr.TeemData.Lock()
					ctlr.TeemData.ResourceType.IPAMVS[ns]++
					ctlr.TeemData.Unlock()
					err := ctlr.processVirtualServers(vs, false)
					if err != nil {
						log.Errorf("Unable to process IPAM entry: %v", pKey)
					}
					break
				}
			}
		case "ts":
			item, exists, err := crInf.tsInformer.GetIndexer().GetByKey(pKey[:idx])
			if !exists || err != nil {
				log.Errorf("Unable to process IPAM entry: %v", pKey)
				continue
			}
			ctlr.TeemData.Lock()
			ctlr.TeemData.ResourceType.IPAMTS[ns]++
			ctlr.TeemData.Unlock()
			ts := item.(*cisapiv1.TransportServer)
			err = ctlr.processTransportServers(ts, false)
			if err != nil {
				log.Errorf("Unable to process IPAM entry: %v", pKey)
			}
		case "il":
			item, exists, err := crInf.ilInformer.GetIndexer().GetByKey(pKey[:idx])
			if !exists || err != nil {
				log.Errorf("Unable to process IPAM entry: %v", pKey)
				continue
			}
			il := item.(*cisapiv1.IngressLink)
			err = ctlr.processIngressLink(il, false)
			if err != nil {
				log.Errorf("Unable to process IPAM entry: %v", pKey)
			}
		case "svc":
			item, exists, err := comInf.svcInformer.GetIndexer().GetByKey(pKey[:idx])
			if !exists || err != nil {
				log.Errorf("Unable to process IPAM entry: %v", pKey)
				continue
			}
			ctlr.TeemData.Lock()
			ctlr.TeemData.ResourceType.IPAMSvcLB[ns]++
			ctlr.TeemData.Unlock()
			svc := item.(*v1.Service)
			err = ctlr.processLBServices(svc, false)
			if err != nil {
				log.Errorf("Unable to process IPAM entry: %v", pKey)
			}
		default:
			log.Errorf("Found Invalid Key: %v while Processing IPAM", pKey)
		}
	}

	return nil
}

func (ctlr *Controller) processIngressLink(
	ingLink *cisapiv1.IngressLink,
	isILDeleted bool,
) error {

	startTime := time.Now()
	defer func() {
		endTime := time.Now()
		log.Debugf("Finished syncing Ingress Links %+v (%v)",
			ingLink, endTime.Sub(startTime))
	}()
	// Skip validation for a deleted ingressLink
	if !isILDeleted {
		// check if the virutal server matches all the requirements.
		vkey := ingLink.ObjectMeta.Namespace + "/" + ingLink.ObjectMeta.Name
		valid := ctlr.checkValidIngressLink(ingLink)
		if false == valid {
			log.Errorf("ingressLink %s, is not valid",
				vkey)
			return nil
		}
	}
	var ip string
	var key string
	var status int
	key = ingLink.ObjectMeta.Namespace + "/" + ingLink.ObjectMeta.Name + "_il"
	if ctlr.ipamCli != nil {
		if isILDeleted && ingLink.Spec.VirtualServerAddress == "" {
			ip = ctlr.releaseIP(ingLink.Spec.IPAMLabel, "", key)
		} else if ingLink.Spec.VirtualServerAddress != "" {
			ip = ingLink.Spec.VirtualServerAddress
		} else {
			ip, status = ctlr.requestIP(ingLink.Spec.IPAMLabel, "", key)

			switch status {
			case NotEnabled:
				log.Debug("IPAM Custom Resource Not Available")
				return nil
			case InvalidInput:
				log.Debugf("IPAM Invalid IPAM Label: %v for IngressLink: %s/%s",
					ingLink.Spec.IPAMLabel, ingLink.Namespace, ingLink.Name)
				return nil
			case NotRequested:
				return fmt.Errorf("unable to make IPAM Request, will be re-requested soon")
			case Requested:
				log.Debugf("IP address requested for IngressLink: %s/%s", ingLink.Namespace, ingLink.Name)
				return nil
			}
			log.Debugf("[ipam] requested IP for ingLink %v is: %v", ingLink.ObjectMeta.Name, ip)
			if ip == "" {
				log.Debugf("[ipam] requested IP for ingLink %v is empty.", ingLink.ObjectMeta.Name)
				return nil
			}
			ctlr.updateIngressLinkStatus(ingLink, ip)
			svc, err := ctlr.getKICServiceOfIngressLink(ingLink)
			if err != nil {
				return err
			}
			if svc == nil {
				return nil
			}
			if svc.Spec.Type == v1.ServiceTypeLoadBalancer {
				ctlr.setLBServiceIngressStatus(svc, ip)
			}
		}
	} else {
		if ingLink.Spec.VirtualServerAddress == "" {
			return fmt.Errorf("No VirtualServer address in ingLink or IPAM found.")
		}
		ip = ingLink.Spec.VirtualServerAddress
	}
	if isILDeleted {
		var delRes []string
		rsMap := ctlr.resources.getPartitionResourceMap(ctlr.Partition)
		for k := range rsMap {
			rsName := "ingress_link_" + formatVirtualServerName(
				ip,
				0,
			)
			if strings.HasPrefix(k, rsName[:len(rsName)-1]) {
				delRes = append(delRes, k)
			}
		}
		for _, rsName := range delRes {
			var hostnames []string
			if rsMap[rsName] != nil {
				rsCfg, err := ctlr.resources.getResourceConfig(ctlr.Partition, rsName)
				if err == nil {
					hostnames = rsCfg.MetaData.hosts
				}
			}
			ctlr.deleteSvcDepResource(rsName, rsMap[rsName])
			ctlr.deleteVirtualServer(ctlr.Partition, rsName)
			if len(hostnames) > 0 {
				ctlr.ProcessAssociatedExternalDNS(hostnames)
			}
		}
		ctlr.TeemData.Lock()
		ctlr.TeemData.ResourceType.IngressLink[ingLink.Namespace]--
		ctlr.TeemData.Unlock()
		return nil
	}
	ctlr.TeemData.Lock()
	ctlr.TeemData.ResourceType.IngressLink[ingLink.Namespace] = len(ctlr.getAllIngressLinks(ingLink.Namespace))
	ctlr.TeemData.Unlock()
	svc, err := ctlr.getKICServiceOfIngressLink(ingLink)
	if err != nil {
		return err
	}

	if svc == nil {
		return nil
	}
	targetPort := nginxMonitorPort
	if ctlr.PoolMemberType == NodePort {
		targetPort = getNodeport(svc, nginxMonitorPort)
		if targetPort == 0 {
			log.Errorf("Nodeport not found for nginx monitor port: %v", nginxMonitorPort)
		}
	}

	rsMap := ctlr.resources.getPartitionResourceMap(ctlr.Partition)
	for _, port := range svc.Spec.Ports {
		//for nginx health monitor port skip vs creation
		if port.Port == nginxMonitorPort {
			continue
		}
		rsName := "ingress_link_" + formatVirtualServerName(
			ip,
			port.Port,
		)

		rsCfg := &ResourceConfig{}
		rsCfg.Virtual.Partition = ctlr.Partition
		rsCfg.MetaData.ResourceType = TransportServer
		rsCfg.MetaData.hosts = append(rsCfg.MetaData.hosts, ingLink.Spec.Host)
		rsCfg.Virtual.Mode = "standard"
		rsCfg.Virtual.TranslateServerAddress = true
		rsCfg.Virtual.TranslateServerPort = true
		rsCfg.Virtual.Source = "0.0.0.0/0"
		rsCfg.Virtual.Enabled = true
		rsCfg.Virtual.Name = rsName
		rsCfg.Virtual.SNAT = DEFAULT_SNAT
		if len(ingLink.Spec.IRules) > 0 {
			rsCfg.Virtual.IRules = ingLink.Spec.IRules
		}
		rsCfg.Virtual.SetVirtualAddress(
			ip,
			port.Port,
		)
		svcPort := intstr.IntOrString{IntVal: port.Port}
		pool := Pool{
			Name: formatPoolName(
				svc.ObjectMeta.Namespace,
				svc.ObjectMeta.Name,
				svcPort,
				"",
				"",
			),
			Partition:        rsCfg.Virtual.Partition,
			ServiceName:      svc.ObjectMeta.Name,
			ServicePort:      svcPort,
			ServiceNamespace: svc.ObjectMeta.Namespace,
		}
		monitorName := fmt.Sprintf("%s_monitor", pool.Name)
		rsCfg.Monitors = append(
			rsCfg.Monitors,
			Monitor{Name: monitorName, Partition: rsCfg.Virtual.Partition, Interval: 20,
				Type: "http", Send: "GET /nginx-ready HTTP/1.1\r\n", Recv: "", Timeout: 10, TargetPort: targetPort})
		pool.MonitorNames = append(pool.MonitorNames, MonitorName{Name: monitorName})
		rsCfg.Virtual.PoolName = pool.Name
		rsCfg.Pools = append(rsCfg.Pools, pool)
		// Update rsMap with ResourceConfigs created for the current ingresslink virtuals
		rsMap[rsName] = rsCfg
		var hostnames []string
		hostnames = rsCfg.MetaData.hosts
		if len(hostnames) > 0 {
			ctlr.ProcessAssociatedExternalDNS(hostnames)
		}

		ctlr.updateSvcDepResources(rsName, rsCfg)

		if ctlr.PoolMemberType == NodePort {
			ctlr.updatePoolMembersForNodePort(rsCfg, ingLink.ObjectMeta.Namespace)
		} else if ctlr.PoolMemberType == NodePortLocal {
			//supported with antrea cni.
			ctlr.updatePoolMembersForNPL(rsCfg, ingLink.ObjectMeta.Namespace)
		} else {
			ctlr.updatePoolMembersForCluster(rsCfg, ingLink.ObjectMeta.Namespace)
		}
	}

	return nil
}

func (ctlr *Controller) getAllIngressLinks(namespace string) []*cisapiv1.IngressLink {
	var allIngLinks []*cisapiv1.IngressLink

	crInf, ok := ctlr.getNamespacedCRInformer(namespace)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", namespace)
		return nil
	}
	var orderedIngLinks []interface{}
	var err error
	if namespace == "" {
		orderedIngLinks = crInf.ilInformer.GetIndexer().List()
	} else {
		// Get list of VirtualServers and process them.
		orderedIngLinks, err = crInf.ilInformer.GetIndexer().ByIndex("namespace", namespace)
		if err != nil {
			log.Errorf("Unable to get list of VirtualServers for namespace '%v': %v",
				namespace, err)
			return nil
		}
	}
	for _, obj := range orderedIngLinks {
		ingLink := obj.(*cisapiv1.IngressLink)
		// TODO
		// Validate the IngressLink List to check if all the vs are valid.

		allIngLinks = append(allIngLinks, ingLink)
	}
	return allIngLinks
}

// getIngressLinksForService gets the List of ingressLink which are effected
// by the addition/deletion/updation of service.
func (ctlr *Controller) getIngressLinksForService(svc *v1.Service) []*cisapiv1.IngressLink {
	ingLinks := ctlr.getAllIngressLinks(svc.ObjectMeta.Namespace)
	ctlr.TeemData.Lock()
	ctlr.TeemData.ResourceType.IngressLink[svc.ObjectMeta.Namespace] = len(ingLinks)
	ctlr.TeemData.Unlock()
	if nil == ingLinks {
		log.Infof("No IngressLink found in namespace %s",
			svc.ObjectMeta.Namespace)
		return nil
	}
	ingresslinksForService := filterIngressLinkForService(ingLinks, svc)

	if nil == ingresslinksForService {
		log.Debugf("Change in Service %s does not effect any IngressLink",
			svc.ObjectMeta.Name)
		return nil
	}

	// Output list of all IngressLinks Found.
	var targetILNames []string
	for _, il := range ingLinks {
		targetILNames = append(targetILNames, il.ObjectMeta.Name)
	}
	log.Debugf("IngressLinks %v are affected with service %s change",
		targetILNames, svc.ObjectMeta.Name)
	// TODO
	// Remove Duplicate entries in the targetILNames.
	// or Add only Unique entries into the targetILNames.
	return ingresslinksForService
}

// filterIngressLinkForService returns list of ingressLinks that are
// affected by the service under process.
func filterIngressLinkForService(allIngressLinks []*cisapiv1.IngressLink,
	svc *v1.Service) []*cisapiv1.IngressLink {

	var result []*cisapiv1.IngressLink
	svcNamespace := svc.ObjectMeta.Namespace

	// find IngressLinks which reference the service
	for _, ingLink := range allIngressLinks {
		if ingLink.ObjectMeta.Namespace != svcNamespace {
			continue
		}
		for k, v := range ingLink.Spec.Selector.MatchLabels {
			if svc.ObjectMeta.Labels[k] == v {
				result = append(result, ingLink)
			}
		}
	}

	return result
}

// get returns list of all ingressLink
func (ctlr *Controller) getAllIngLinkFromMonitoredNamespaces() []*cisapiv1.IngressLink {
	var allInglink []*cisapiv1.IngressLink
	if ctlr.watchingAllNamespaces() {
		return ctlr.getAllIngressLinks("")
	}
	for ns := range ctlr.namespaces {
		allInglink = append(allInglink, ctlr.getAllIngressLinks(ns)...)
	}
	return allInglink
}

func (ctlr *Controller) getKICServiceOfIngressLink(ingLink *cisapiv1.IngressLink) (*v1.Service, error) {
	selector := ""
	for k, v := range ingLink.Spec.Selector.MatchLabels {
		selector += fmt.Sprintf("%v=%v,", k, v)
	}
	selector = selector[:len(selector)-1]

	comInf, ok := ctlr.getNamespacedCommonInformer(ingLink.ObjectMeta.Namespace)
	if !ok {
		return nil, fmt.Errorf("informer not found for namepsace %v", ingLink.ObjectMeta.Namespace)
	}
	ls, _ := createLabel(selector)
	serviceList, err := listerscorev1.NewServiceLister(comInf.svcInformer.GetIndexer()).Services(ingLink.ObjectMeta.Namespace).List(ls)

	if err != nil {
		log.Errorf("Error getting service list From IngressLink. Error: %v", err)
		return nil, err
	}

	if len(serviceList) == 0 {
		log.Infof("No services for with labels : %v", ingLink.Spec.Selector.MatchLabels)
		return nil, nil
	}

	if len(serviceList) == 1 {
		return serviceList[0], nil
	}

	sort.Sort(Services(serviceList))
	return serviceList[0], nil
}

func (ctlr *Controller) setLBServiceIngressStatus(
	svc *v1.Service,
	ip string,
) {
	// Set the ingress status to include the virtual IP
	lbIngress := v1.LoadBalancerIngress{IP: ip}
	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		svc.Status.LoadBalancer.Ingress = append(svc.Status.LoadBalancer.Ingress, lbIngress)
	} else if svc.Status.LoadBalancer.Ingress[0].IP != ip {
		svc.Status.LoadBalancer.Ingress[0] = lbIngress
	}

	_, updateErr := ctlr.kubeClient.CoreV1().Services(svc.ObjectMeta.Namespace).UpdateStatus(context.TODO(), svc, metav1.UpdateOptions{})
	if nil != updateErr {
		// Multi-service causes the controller to try to update the status multiple times
		// at once. Ignore this error.
		if strings.Contains(updateErr.Error(), "object has been modified") {
			return
		}
		warning := fmt.Sprintf(
			"Error when setting Service LB Ingress status IP: %v", updateErr)
		log.Warning(warning)
		ctlr.recordLBServiceIngressEvent(svc, v1.EventTypeWarning, "StatusIPError", warning)
	} else {
		message := fmt.Sprintf("F5 CIS assigned LoadBalancer IP: %v", ip)
		ctlr.recordLBServiceIngressEvent(svc, v1.EventTypeNormal, "ExternalIP", message)
	}
}

func (ctlr *Controller) unSetLBServiceIngressStatus(
	svc *v1.Service,
	ip string,
) {

	svcName := svc.Namespace + "/" + svc.Name
	comInf, _ := ctlr.getNamespacedCommonInformer(svc.Namespace)
	service, found, err := comInf.svcInformer.GetIndexer().GetByKey(svcName)
	if !found || err != nil {
		log.Debugf("Unable to Update Status of Service: %v due to unavailability", svcName)
		return
	}
	svc = service.(*v1.Service)
	index := -1
	for i, lbIng := range svc.Status.LoadBalancer.Ingress {
		if lbIng.IP == ip {
			index = i
			break
		}
	}

	if index != -1 {
		svc.Status.LoadBalancer.Ingress = append(svc.Status.LoadBalancer.Ingress[:index],
			svc.Status.LoadBalancer.Ingress[index+1:]...)

		_, updateErr := ctlr.kubeClient.CoreV1().Services(svc.ObjectMeta.Namespace).UpdateStatus(
			context.TODO(), svc, metav1.UpdateOptions{})
		if nil != updateErr {
			// Multi-service causes the controller to try to update the status multiple times
			// at once. Ignore this error.
			if strings.Contains(updateErr.Error(), "object has been modified") {
				log.Debugf("Error while updating service: %v. %v", svcName, updateErr.Error())
				return
			}
			warning := fmt.Sprintf(
				"Error when unsetting Service LB Ingress status IP: %v", updateErr)
			log.Warning(warning)
			ctlr.recordLBServiceIngressEvent(svc, v1.EventTypeWarning, "StatusIPError", warning)
		} else {
			message := fmt.Sprintf("F5 CIS unassigned LoadBalancer IP: %v", ip)
			ctlr.recordLBServiceIngressEvent(svc, v1.EventTypeNormal, "ExternalIP", message)
		}
	}
}

//func (ctlr *Controller) eraseLBServiceIngressStatus(
//	svc *v1.Service,
//) {
//	svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{}
//
//	_, updateErr := ctlr.kubeClient.CoreV1().Services(svc.ObjectMeta.Namespace).UpdateStatus(
//		context.TODO(), svc, metav1.UpdateOptions{})
//	if nil != updateErr {
//		// Multi-service causes the controller to try to update the status multiple times
//		// at once. Ignore this error.
//		if strings.Contains(updateErr.Error(), "object has been modified") {
//			log.Debugf("Error while updating service: %v/%v. %v", svc.Namespace, svc.Name, updateErr.Error())
//			return
//		}
//		warning := fmt.Sprintf(
//			"Error when erasing Service LB Ingress status IP: %v", updateErr)
//		log.Warning(warning)
//		ctlr.recordLBServiceIngressEvent(svc, v1.EventTypeWarning, "StatusIPError", warning)
//	} else {
//		message := fmt.Sprintf("F5 CIS erased LoadBalancer IP in Status")
//		ctlr.recordLBServiceIngressEvent(svc, v1.EventTypeNormal, "ExternalIP", message)
//	}
//}

func (ctlr *Controller) recordLBServiceIngressEvent(
	svc *v1.Service,
	eventType string,
	reason string,
	message string,
) {
	namespace := svc.ObjectMeta.Namespace
	// Create the event
	evNotifier := ctlr.eventNotifier.CreateNotifierForNamespace(
		namespace, ctlr.kubeClient.CoreV1())
	evNotifier.RecordEvent(svc, eventType, reason, message)
}

// sort services by timestamp
func (svcs Services) Len() int {
	return len(svcs)
}

func (svcs Services) Less(i, j int) bool {
	d1 := svcs[i].GetCreationTimestamp()
	d2 := svcs[j].GetCreationTimestamp()
	return d1.Before(&d2)
}

func (svcs Services) Swap(i, j int) {
	svcs[i], svcs[j] = svcs[j], svcs[i]
}

// sort Nodes by Name
func (nodes NodeList) Len() int {
	return len(nodes)
}

func (nodes NodeList) Less(i, j int) bool {
	return nodes[i].Name < nodes[j].Name
}

func (nodes NodeList) Swap(i, j int) {
	nodes[i], nodes[j] = nodes[j], nodes[i]
}

func getNodeport(svc *v1.Service, servicePort int32) int32 {
	for _, port := range svc.Spec.Ports {
		if port.Port == servicePort {
			return port.NodePort
		}
	}
	return 0
}

// Update virtual server status with virtual server address
func (ctlr *Controller) updateVirtualServerStatus(vs *cisapiv1.VirtualServer, ip string, statusOk string) {
	// Set the vs status to include the virtual IP address
	vsStatus := cisapiv1.VirtualServerStatus{VSAddress: ip, StatusOk: statusOk}
	log.Debugf("Updating VirtualServer Status with %v for resource name:%v , namespace: %v", vsStatus, vs.Name, vs.Namespace)
	vs.Status = vsStatus
	vs.Status.VSAddress = ip
	vs.Status.StatusOk = statusOk
	_, updateErr := ctlr.kubeCRClient.CisV1().VirtualServers(vs.ObjectMeta.Namespace).UpdateStatus(context.TODO(), vs, metav1.UpdateOptions{})
	if nil != updateErr {
		log.Debugf("Error while updating virtual server status:%v", updateErr)
		return
	}
}

// Update Transport server status with virtual server address
func (ctlr *Controller) updateTransportServerStatus(ts *cisapiv1.TransportServer, ip string, statusOk string) {
	// Set the vs status to include the virtual IP address
	tsStatus := cisapiv1.TransportServerStatus{VSAddress: ip, StatusOk: statusOk}
	log.Debugf("Updating VirtualServer Status with %v for resource name:%v , namespace: %v", tsStatus, ts.Name, ts.Namespace)
	ts.Status = tsStatus
	ts.Status.VSAddress = ip
	ts.Status.StatusOk = statusOk
	_, updateErr := ctlr.kubeCRClient.CisV1().TransportServers(ts.ObjectMeta.Namespace).UpdateStatus(context.TODO(), ts, metav1.UpdateOptions{})
	if nil != updateErr {
		log.Debugf("Error while updating Transport server status:%v", updateErr)
		return
	}
}

// Update ingresslink status with virtual server address
func (ctlr *Controller) updateIngressLinkStatus(il *cisapiv1.IngressLink, ip string) {
	// Set the vs status to include the virtual IP address
	ilStatus := cisapiv1.IngressLinkStatus{VSAddress: ip}
	il.Status = ilStatus
	_, updateErr := ctlr.kubeCRClient.CisV1().IngressLinks(il.ObjectMeta.Namespace).UpdateStatus(context.TODO(), il, metav1.UpdateOptions{})
	if nil != updateErr {
		log.Debugf("Error while updating ingresslink status:%v", updateErr)
		return
	}
}

// returns service obj with servicename
func (ctlr *Controller) GetService(namespace, serviceName string) *v1.Service {
	svcKey := namespace + "/" + serviceName
	comInf, ok := ctlr.getNamespacedCommonInformer(namespace)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", namespace)
		return nil
	}
	svc, found, err := comInf.svcInformer.GetIndexer().GetByKey(svcKey)
	if err != nil {
		log.Infof("Error fetching service %v from the store: %v", svcKey, err)
		return nil
	}
	if !found {
		log.Errorf("Error: Service %v not found", svcKey)
		return nil
	}
	return svc.(*v1.Service)
}

// GetPodsForService returns podList with labels set to svc selector
func (ctlr *Controller) GetPodsForService(namespace, serviceName string, nplAnnotationRequired bool) []*v1.Pod {
	svcKey := namespace + "/" + serviceName
	comInf, ok := ctlr.getNamespacedCommonInformer(namespace)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", namespace)
		return nil
	}
	svc, found, err := comInf.svcInformer.GetIndexer().GetByKey(svcKey)
	if err != nil {
		log.Infof("Error fetching service %v from the store: %v", svcKey, err)
		return nil
	}
	if !found {
		log.Errorf("Error: Service %v not found", svcKey)
		return nil
	}
	annotations := svc.(*v1.Service).Annotations
	if _, ok := annotations[NPLSvcAnnotation]; !ok && nplAnnotationRequired {
		log.Errorf("NPL annotation %v not set on service %v", NPLSvcAnnotation, serviceName)
		return nil
	}

	selector := svc.(*v1.Service).Spec.Selector
	if len(selector) == 0 {
		log.Infof("label selector is not set on svc")
		return nil
	}
	labelSelector, err := metav1.ParseToLabelSelector(labels.Set(selector).AsSelectorPreValidated().String())
	labelmap, err := metav1.LabelSelectorAsMap(labelSelector)
	if err != nil {
		return nil
	}
	pl, _ := createLabel(labels.SelectorFromSet(labelmap).String())
	podList, err := listerscorev1.NewPodLister(comInf.podInformer.GetIndexer()).Pods(namespace).List(pl)
	if err != nil {
		log.Debugf("Got error while listing Pods with selector %v: %v", selector, err)
		return nil
	}
	return podList
}

func (ctlr *Controller) GetServicesForPod(pod *v1.Pod) *v1.Service {
	comInf, ok := ctlr.getNamespacedCommonInformer(pod.Namespace)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", pod.Namespace)
		return nil
	}
	services, err := comInf.svcInformer.GetIndexer().ByIndex("namespace", pod.Namespace)
	if err != nil {
		log.Debugf("Unable to find services for namespace %v with error: %v", pod.Namespace, err)
	}
	for _, obj := range services {
		svc := obj.(*v1.Service)
		if svc.Spec.Type != v1.ServiceTypeNodePort {
			if ctlr.matchSvcSelectorPodLabels(svc.Spec.Selector, pod.GetLabels()) {
				return svc
			}
		}
	}
	return nil
}

func (ctlr *Controller) matchSvcSelectorPodLabels(svcSelector, podLabel map[string]string) bool {
	if len(svcSelector) == 0 {
		return false
	}

	for selectorKey, selectorVal := range svcSelector {
		if labelVal, ok := podLabel[selectorKey]; !ok || selectorVal != labelVal {
			return false
		}
	}
	return true
}

// processPod populates NPL annotations for a pod in store.
func (ctlr *Controller) processPod(pod *v1.Pod, ispodDeleted bool) error {
	podKey := pod.Namespace + "/" + pod.Name
	if ispodDeleted {
		delete(ctlr.resources.nplStore, podKey)
		return nil
	}
	ann := pod.GetAnnotations()
	var annotations []NPLAnnotation
	if val, ok := ann[NPLPodAnnotation]; ok {
		if err := json.Unmarshal([]byte(val), &annotations); err != nil {
			log.Errorf("key: %s, got error while unmarshaling NPL annotations: %v", podKey, err)
		}
		ctlr.resources.nplStore[podKey] = annotations
	} else {
		log.Debugf("key: %s, NPL annotation not found for Pod", pod.Name)
		delete(ctlr.resources.nplStore, podKey)
	}
	return nil
}

// getPolicyFromLBService gets the policy attached to the service and returns it
func (ctlr *Controller) getPolicyFromLBService(svc *v1.Service) (*cisapiv1.Policy, error) {
	plcName, found := svc.Annotations[LBServicePolicyNameAnnotation]
	if !found || plcName == "" {
		return nil, nil
	}
	ns := svc.Namespace
	return ctlr.getPolicy(ns, plcName)
}

// skipVirtual return true if virtuals don't have any common HTTP/HTTPS ports, else returns false
func skipVirtual(currentVS *cisapiv1.VirtualServer, vrt *cisapiv1.VirtualServer) bool {
	effectiveCurrentVSHTTPSPort := getEffectiveHTTPSPort(currentVS)
	effectiveVrtVSHTTPSPort := getEffectiveHTTPSPort(vrt)
	effectiveCurrentVSHTTPPort := getEffectiveHTTPPort(currentVS)
	effectiveVrtVSHTTPPort := getEffectiveHTTPPort(vrt)
	if effectiveCurrentVSHTTPSPort == effectiveVrtVSHTTPSPort && effectiveCurrentVSHTTPPort == effectiveVrtVSHTTPPort {
		// both virtuals use same ports
		return false
	}
	if effectiveCurrentVSHTTPSPort != effectiveVrtVSHTTPSPort && effectiveCurrentVSHTTPPort != effectiveVrtVSHTTPPort {
		// virtuals don't have any port in common
		return true
	}
	if effectiveCurrentVSHTTPSPort == effectiveVrtVSHTTPSPort && effectiveCurrentVSHTTPPort != effectiveVrtVSHTTPPort {
		// virtuals have HTTPS port is common
		if currentVS.Spec.TLSProfileName == "" || vrt.Spec.TLSProfileName == "" {
			// One of the vs is an unsecured vs so common HTTPS port is insignificant for this vs
			return true
		}
		// both vs are secured vs and have common HTTPS port
		return false
	}

	// virtuals have HTTP port in common
	// setting HTTPTraffic="" for both none and "" value for HTTPTraffic to simplify comparison
	currentVSHTTPTraffic := currentVS.Spec.HTTPTraffic
	vrtVSHTTPTraffic := vrt.Spec.HTTPTraffic
	if currentVSHTTPTraffic == "" || currentVSHTTPTraffic == "none" {
		currentVSHTTPTraffic = ""
	}
	if vrtVSHTTPTraffic == "" || vrtVSHTTPTraffic == "none" {
		vrtVSHTTPTraffic = ""
	}
	if currentVS.Spec.TLSProfileName != "" && vrt.Spec.TLSProfileName != "" {
		if currentVSHTTPTraffic == "" || vrtVSHTTPTraffic == "" {
			// both vs are secured vs but one/both of them doesn't handle HTTP traffic so common HTTP port is insignificant
			return true
		}
		// both vs are secured and both of them handle HTTP traffic via the common HTTP port
		return false
	}
	if currentVS.Spec.TLSProfileName != "" && currentVSHTTPTraffic == "" {
		// current vs is secured vs, and it doesn't handle HTTP traffic so common HTTP port is insignificant
		return true
	}
	if vrt.Spec.TLSProfileName != "" && vrtVSHTTPTraffic == "" {
		// It's a secured vs, and it doesn't handle HTTP traffic so common HTTP port is insignificant
		return true
	}
	// Either both are unsecured vs and have common HTTP port
	// or one of them is a secured vs and handles HTTP traffic through the common port
	return false
}

// doVSUseSameHTTPSPort checks if any of the associated secured VS uses the same HTTPS port that the current VS does
func doVSUseSameHTTPSPort(virtuals []*cisapiv1.VirtualServer, currentVirtual *cisapiv1.VirtualServer) bool {
	effectiveCurrentVSHTTPSPort := getEffectiveHTTPSPort(currentVirtual)
	for _, virtual := range virtuals {
		if virtual.Spec.TLSProfileName != "" && effectiveCurrentVSHTTPSPort == getEffectiveHTTPSPort(virtual) {
			return true
		}
	}
	return false
}

func fetchPortString(port intstr.IntOrString) string {
	if port.StrVal != "" {
		return port.StrVal
	}
	if port.IntVal != 0 {
		return fmt.Sprintf("%v", port.IntVal)
	}
	return ""
}

// fetch list of tls profiles for given secret.
func (ctlr *Controller) getTLSProfilesForSecret(secret *v1.Secret) []*cisapiv1.TLSProfile {
	var allTLSProfiles []*cisapiv1.TLSProfile

	crInf, ok := ctlr.getNamespacedCRInformer(secret.Namespace)
	if !ok {
		log.Errorf("Informer not found for namespace: %v", secret.Namespace)
		return nil
	}

	var orderedTLS []interface{}
	var err error
	orderedTLS, err = crInf.tlsInformer.GetIndexer().ByIndex("namespace", secret.Namespace)
	if err != nil {
		log.Errorf("Unable to get list of TLS Profiles for namespace '%v': %v",
			secret.Namespace, err)
		return nil
	}

	for _, obj := range orderedTLS {
		tlsProfile := obj.(*cisapiv1.TLSProfile)
		if tlsProfile.Spec.TLS.Reference == Secret {
			if len(tlsProfile.Spec.TLS.ClientSSLs) > 0 {
				for _, name := range tlsProfile.Spec.TLS.ClientSSLs {
					if name == secret.Name {
						allTLSProfiles = append(allTLSProfiles, tlsProfile)
					}
				}
			} else if tlsProfile.Spec.TLS.ClientSSL == secret.Name {
				allTLSProfiles = append(allTLSProfiles, tlsProfile)
			}
		}
	}
	return allTLSProfiles
}

func createLabel(label string) (labels.Selector, error) {
	var l labels.Selector
	var err error
	if label == "" {
		l = labels.Everything()
	} else {
		l, err = labels.Parse(label)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Label Selector string: %v", err)
		}
	}
	return l, nil
}
