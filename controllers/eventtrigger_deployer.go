/*
Copyright 2023. projectsveltos.io. All rights reserved.

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

package controllers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"text/template"

	"github.com/gdexlab/go-render/render"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1beta1 "github.com/projectsveltos/addon-controller/api/v1beta1"
	"github.com/projectsveltos/event-manager/api/v1beta1"
	"github.com/projectsveltos/event-manager/pkg/scope"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/libsveltos/lib/clusterproxy"
	"github.com/projectsveltos/libsveltos/lib/deployer"
	"github.com/projectsveltos/libsveltos/lib/funcmap"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/libsveltos/lib/sharding"
	libsveltostemplate "github.com/projectsveltos/libsveltos/lib/template"
	libsveltosutils "github.com/projectsveltos/libsveltos/lib/utils"
)

const (
	// Namespace where reports will be generated
	ReportNamespace = "projectsveltos"

	eventReportNameLabel             = "eventtrigger.lib.projectsveltos.io/eventreportname"
	eventTriggerNameLabel            = "eventtrigger.lib.projectsveltos.io/eventtriggername"
	clusterNamespaceLabel            = "eventtrigger.lib.projectsveltos.io/clusterNamespace"
	clusterNameLabel                 = "eventtrigger.lib.projectsveltos.io/clustername"
	clusterTypeLabel                 = "eventtrigger.lib.projectsveltos.io/clustertype"
	referencedResourceNamespaceLabel = "eventtrigger.lib.projectsveltos.io/refnamespace"
	referencedResourceNameLabel      = "eventtrigger.lib.projectsveltos.io/refname"
)

type getCurrentHash func(tx context.Context, c client.Client,
	chc *v1beta1.EventTrigger, cluster *corev1.ObjectReference, logger logr.Logger) ([]byte, error)

type feature struct {
	id          string
	currentHash getCurrentHash
	deploy      deployer.RequestHandler
	undeploy    deployer.RequestHandler
}

func (r *EventTriggerReconciler) isClusterAShardMatch(ctx context.Context,
	clusterInfo *libsveltosv1beta1.ClusterInfo) (bool, error) {

	clusterType := clusterproxy.GetClusterType(&clusterInfo.Cluster)
	cluster, err := clusterproxy.GetCluster(ctx, r.Client, clusterInfo.Cluster.Namespace,
		clusterInfo.Cluster.Name, clusterType)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}

	return sharding.IsShardAMatch(r.ShardKey, cluster), nil
}

// deployEventBasedAddon update necessary resources (eventSource) in the managed clusters
func (r *EventTriggerReconciler) deployEventTrigger(ctx context.Context, eScope *scope.EventTriggerScope,
	f feature, logger logr.Logger) error {

	resource := eScope.EventTrigger

	logger = logger.WithValues("eventTrigger", resource.Name)
	logger.V(logs.LogDebug).Info("request to evaluate/deploy")

	var errorSeen error
	allProcessed := true

	for i := range resource.Status.ClusterInfo {
		c := &resource.Status.ClusterInfo[i]

		shardMatch, err := r.isClusterAShardMatch(ctx, c)
		if err != nil {
			return err
		}

		var clusterInfo *libsveltosv1beta1.ClusterInfo
		if !shardMatch {
			l := logger.WithValues("cluster", fmt.Sprintf("%s:%s/%s",
				c.Cluster.Kind, c.Cluster.Namespace, c.Cluster.Name))
			l.V(logs.LogDebug).Info("cluster is not a shard match")
			// Since cluster is not a shard match, another deployment will deploy and update
			// this specific clusterInfo status. Here we simply return current status.
			clusterInfo = c
			if clusterInfo.Status != libsveltosv1beta1.SveltosStatusProvisioned {
				allProcessed = false
			}
			// This is a required parameter. It is set by the deployment matching the
			// cluster shard. if not set yet, set it to empty
			if clusterInfo.Hash == nil {
				str := base64.StdEncoding.EncodeToString([]byte("empty"))
				clusterInfo.Hash = []byte(str)
			}
		} else {
			clusterInfo, err = r.processEventTrigger(ctx, eScope, &c.Cluster, f, logger)
			if err != nil {
				errorSeen = err
			}
			if clusterInfo != nil {
				resource.Status.ClusterInfo[i] = *clusterInfo
				if clusterInfo.Status != libsveltosv1beta1.SveltosStatusProvisioned {
					allProcessed = false
				}
			}
		}
	}

	logger.V(logs.LogDebug).Info("set clusterInfo")
	eScope.SetClusterInfo(resource.Status.ClusterInfo)

	if errorSeen != nil {
		return errorSeen
	}

	if !allProcessed {
		return fmt.Errorf("request to process EventTrigger is still queued in one ore more clusters")
	}

	return nil
}

// undeployEventBasedAddon clean resources in managed clusters
func (r *EventTriggerReconciler) undeployEventTrigger(ctx context.Context, eScope *scope.EventTriggerScope,
	clusterInfo []libsveltosv1beta1.ClusterInfo, logger logr.Logger) error {

	f := getHandlersForFeature(v1beta1.FeatureEventTrigger)

	resource := eScope.EventTrigger

	logger = logger.WithValues("eventTrigger", resource.Name)
	logger.V(logs.LogDebug).Info("request to undeploy")

	var err error
	for i := range clusterInfo {
		shardMatch, tmpErr := r.isClusterAShardMatch(ctx, &clusterInfo[i])
		if tmpErr != nil {
			err = tmpErr
			continue
		}

		if !shardMatch && clusterInfo[i].Status != libsveltosv1beta1.SveltosStatusRemoved {
			// If shard is not a match, wait for other controller to remove
			err = fmt.Errorf("remove pending")
			continue
		}

		c := &clusterInfo[i].Cluster

		logger.V(logs.LogDebug).Info(fmt.Sprintf("undeploy EventTrigger from cluster %s:%s/%s",
			c.Kind, c.Namespace, c.Name))
		_, tmpErr = r.removeEventTrigger(ctx, eScope, c, f, logger)
		if tmpErr != nil {
			err = tmpErr
			continue
		}

		clusterInfo[i].Status = libsveltosv1beta1.SveltosStatusRemoved
	}

	return err
}

// processEventTriggerForCluster deploys necessary resources in managed cluster.
func processEventTriggerForCluster(ctx context.Context, c client.Client,
	clusterNamespace, clusterName, applicant, featureID string,
	clusterType libsveltosv1beta1.ClusterType, options deployer.Options, logger logr.Logger) error {

	logger = logger.WithValues("eventTrigger", applicant)
	logger = logger.WithValues("cluster", fmt.Sprintf("%s:%s/%s", clusterType, clusterNamespace, clusterName))

	resource := &v1beta1.EventTrigger{}
	err := c.Get(ctx, types.NamespacedName{Name: applicant}, resource)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(logs.LogDebug).Info("eventTrigger not found")
			return nil
		}
		return err
	}

	if !resource.DeletionTimestamp.IsZero() {
		logger.V(logs.LogDebug).Info("eventTrigger marked for deletion")
		return nil
	}

	logger.V(logs.LogDebug).Info("Deploy eventTrigger")

	err = deployEventSource(ctx, c, clusterNamespace, clusterName, clusterType, resource, logger)
	if err != nil {
		logger.V(logs.LogDebug).Info("failed to deploy referenced EventSource")
		return err
	}

	err = removeStaleEventSources(ctx, c, clusterNamespace, clusterName, clusterType, resource, false, logger)
	if err != nil {
		logger.V(logs.LogDebug).Info("failed to remove stale EventSources")
		return err
	}

	logger.V(logs.LogDebug).Info("Deployed eventTrigger")
	return nil
}

// undeployEventTriggerResourcesFromCluster cleans resources associtated with EventTrigger instance:
// - resources (EventSource) from managed cluster
// - resources (EventReports) from the management cluster (those were pulled from the managed cluster)
// - resources instantiated in the management cluster (ConfigMap/Secrets expressed as templated referenced
// in PolicyRefs/ValuesFrom sections)
func undeployEventTriggerResourcesFromCluster(ctx context.Context, c client.Client,
	clusterNamespace, clusterName, applicant, featureID string,
	clusterType libsveltosv1beta1.ClusterType, options deployer.Options, logger logr.Logger) error {

	logger = logger.WithValues("eventTrigger", applicant)

	resource := &v1beta1.EventTrigger{}
	err := c.Get(ctx, types.NamespacedName{Name: applicant}, resource)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(logs.LogDebug).Info("eventTrigger not found")
			return nil
		}
		return err
	}

	logger = logger.WithValues("cluster", fmt.Sprintf("%s:%s/%s", clusterType, clusterNamespace, clusterName))
	logger.V(logs.LogDebug).Info("Undeploy eventTrigger")

	err = removeStaleEventSources(ctx, c, clusterNamespace, clusterName, clusterType, resource, true, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to remove eventSources: %v", err))
		return err
	}

	logger.V(logs.LogDebug).Info("Undeployed eventTrigger.")

	logger.V(logs.LogDebug).Info("Clearing instantiated ClusterProfile/ConfigMap/Secret instances")
	return removeInstantiatedResources(ctx, c, clusterNamespace, clusterName, clusterType, resource,
		nil, nil, logger)
}

// eventTriggerHash returns the EventTrigger hash
func eventTriggerHash(ctx context.Context, c client.Client,
	e *v1beta1.EventTrigger, cluster *corev1.ObjectReference, logger logr.Logger) ([]byte, error) {

	resources, err := fetchReferencedResources(ctx, c, e, cluster, logger)
	if err != nil {
		return nil, err
	}

	config := render.AsCode(e.Spec)
	config += render.AsCode(e.Labels)

	for i := range resources {
		switch r := resources[i].(type) {
		case *corev1.ConfigMap:
			config += render.AsCode(r.Data)
		case *corev1.Secret:
			config += render.AsCode(r.Data)
		case *libsveltosv1beta1.EventSource:
			config += render.AsCode(r.Spec)
		case *libsveltosv1beta1.EventReport:
			config += render.AsCode(r.Spec)
		default:
			panic(1)
		}
	}

	h := sha256.New()
	h.Write([]byte(config))
	return h.Sum(nil), nil
}

// processEventTrigger detects whether it is needed to deploy EventBasedAddon resources in current passed cluster.
func (r *EventTriggerReconciler) processEventTrigger(ctx context.Context, eScope *scope.EventTriggerScope,
	cluster *corev1.ObjectReference, f feature, logger logr.Logger,
) (*libsveltosv1beta1.ClusterInfo, error) {

	if !isClusterStillMatching(eScope, cluster) {
		return r.removeEventTrigger(ctx, eScope, cluster, f, logger)
	}

	resource := eScope.EventTrigger

	// Get EventTrigger Spec hash (at this very precise moment)
	currentHash, err := eventTriggerHash(ctx, r.Client, resource, cluster, logger)
	if err != nil {
		return nil, err
	}

	proceed, err := r.canProceed(ctx, eScope, cluster, logger)
	if err != nil {
		return nil, err
	} else if !proceed {
		return nil, nil
	}

	// Remove any queued entry to cleanup
	r.Deployer.CleanupEntries(cluster.Namespace, cluster.Name, resource.Name, f.id,
		clusterproxy.GetClusterType(cluster), true)

	// If undeploying feature is in progress, wait for it to complete.
	// Otherwise, if we redeploy feature while same feature is still being cleaned up, if two workers process those request in
	// parallel some resources might end up missing.
	if r.Deployer.IsInProgress(cluster.Namespace, cluster.Name, resource.Name, f.id, clusterproxy.GetClusterType(cluster), true) {
		logger.V(logs.LogDebug).Info("cleanup is in progress")
		return nil, fmt.Errorf("cleanup of %s in cluster still in progress. Wait before redeploying", f.id)
	}

	// Get the EventTrigger hash when EventTrigger was last deployed/evaluated in this cluster (if ever)
	hash, currentStatus := r.getClusterHashAndStatus(resource, cluster)
	isConfigSame := reflect.DeepEqual(hash, currentHash)
	if !isConfigSame {
		logger.V(logs.LogDebug).Info(fmt.Sprintf("EventTrigger has changed. Current hash %x. Previous hash %x",
			currentHash, hash))
	}

	var status *libsveltosv1beta1.SveltosFeatureStatus
	var result deployer.Result

	if isConfigSame {
		logger.V(logs.LogInfo).Info("EventTrigger has not changed")
		result = r.Deployer.GetResult(ctx, cluster.Namespace, cluster.Name, resource.Name, f.id,
			clusterproxy.GetClusterType(cluster), false)
		status = r.convertResultStatus(result)
	}

	if status != nil {
		logger.V(logs.LogDebug).Info(fmt.Sprintf("result is available %q. updating status.", *status))
		var errorMessage string
		if result.Err != nil {
			errorMessage = result.Err.Error()
		}
		clusterInfo := &libsveltosv1beta1.ClusterInfo{
			Cluster:        *cluster,
			Status:         *status,
			Hash:           currentHash,
			FailureMessage: &errorMessage,
		}

		if *status == libsveltosv1beta1.SveltosStatusProvisioned {
			return clusterInfo, nil
		}
		if *status == libsveltosv1beta1.SveltosStatusProvisioning {
			return clusterInfo, fmt.Errorf("EventTrigger is still being provisioned")
		}
	} else if isConfigSame && currentStatus != nil && *currentStatus == libsveltosv1beta1.SveltosStatusProvisioned {
		logger.V(logs.LogInfo).Info("already deployed")
		s := libsveltosv1beta1.SveltosStatusProvisioned
		status = &s
	} else {
		logger.V(logs.LogInfo).Info("no result is available. queue job and mark status as provisioning")
		s := libsveltosv1beta1.SveltosStatusProvisioning
		status = &s

		// Getting here means either EventTrigger failed to be deployed or EventTrigger has changed.
		// EventTrigger must be (re)deployed.
		if err := r.Deployer.Deploy(ctx, cluster.Namespace, cluster.Name, resource.Name, f.id, clusterproxy.GetClusterType(cluster),
			false, processEventTriggerForCluster, programDuration, deployer.Options{}); err != nil {
			return nil, err
		}
	}

	clusterInfo := &libsveltosv1beta1.ClusterInfo{
		Cluster:        *cluster,
		Status:         *status,
		Hash:           currentHash,
		FailureMessage: nil,
	}

	return clusterInfo, nil
}

func (r *EventTriggerReconciler) removeEventTrigger(ctx context.Context, eScope *scope.EventTriggerScope,
	cluster *corev1.ObjectReference, f feature, logger logr.Logger) (*libsveltosv1beta1.ClusterInfo, error) {

	resource := eScope.EventTrigger

	logger = logger.WithValues("eventTrigger", resource.Name)
	logger.V(logs.LogDebug).Info("request to undeploy")

	// Remove any queued entry to deploy/evaluate
	r.Deployer.CleanupEntries(cluster.Namespace, cluster.Name, resource.Name, f.id,
		clusterproxy.GetClusterType(cluster), false)

	// If deploying feature is in progress, wait for it to complete.
	// Otherwise, if we cleanup feature while same feature is still being provisioned, if two workers process those request in
	// parallel some resources might be left over.
	if r.Deployer.IsInProgress(cluster.Namespace, cluster.Name, resource.Name, f.id,
		clusterproxy.GetClusterType(cluster), false) {

		logger.V(logs.LogDebug).Info("provisioning is in progress")
		return nil, fmt.Errorf("deploying %s still in progress. Wait before cleanup", f.id)
	}

	if r.isClusterEntryRemoved(resource, cluster) {
		logger.V(logs.LogDebug).Info("feature is removed")
		// feature is removed. Nothing to do.
		return nil, nil
	}

	result := r.Deployer.GetResult(ctx, cluster.Namespace, cluster.Name, resource.Name, f.id,
		clusterproxy.GetClusterType(cluster), true)
	status := r.convertResultStatus(result)

	clusterInfo := &libsveltosv1beta1.ClusterInfo{
		Cluster: *cluster,
		Status:  libsveltosv1beta1.SveltosStatusRemoving,
		Hash:    nil,
	}

	if status != nil {
		if *status == libsveltosv1beta1.SveltosStatusRemoving {
			return clusterInfo, fmt.Errorf("feature is still being removed")
		}

		if *status == libsveltosv1beta1.SveltosStatusRemoved {
			logger.V(logs.LogDebug).Info("status is removed")
			if err := removeClusterInfoEntry(ctx, r.Client, cluster.Namespace, cluster.Name,
				clusterproxy.GetClusterType(cluster), resource, logger); err != nil {
				return nil, err
			}
			clusterInfo.Status = libsveltosv1beta1.SveltosStatusRemoved
			return clusterInfo, nil
		}
	} else {
		logger.V(logs.LogDebug).Info("no result is available. mark status as removing")
	}

	logger.V(logs.LogDebug).Info("queueing request to un-deploy")
	if err := r.Deployer.Deploy(ctx, cluster.Namespace, cluster.Name, resource.Name, f.id,
		clusterproxy.GetClusterType(cluster), true,
		undeployEventTriggerResourcesFromCluster, programDuration, deployer.Options{}); err != nil {
		return nil, err
	}

	return clusterInfo, fmt.Errorf("cleanup request is queued")
}

// isClusterEntryRemoved returns true if feature is there is no entry for cluster in Status.ClusterInfo
func (r *EventTriggerReconciler) isClusterEntryRemoved(resource *v1beta1.EventTrigger,
	cluster *corev1.ObjectReference) bool {

	for i := range resource.Status.ClusterInfo {
		cc := &resource.Status.ClusterInfo[i]
		if isClusterInfoForCluster(cc, cluster.Namespace, cluster.Name, clusterproxy.GetClusterType(cluster)) {
			return false
		}
	}
	return true
}

func (r *EventTriggerReconciler) convertResultStatus(result deployer.Result) *libsveltosv1beta1.SveltosFeatureStatus {
	switch result.ResultStatus {
	case deployer.Deployed:
		s := libsveltosv1beta1.SveltosStatusProvisioned
		return &s
	case deployer.Failed:
		s := libsveltosv1beta1.SveltosStatusFailed
		return &s
	case deployer.InProgress:
		s := libsveltosv1beta1.SveltosStatusProvisioning
		return &s
	case deployer.Removed:
		s := libsveltosv1beta1.SveltosStatusRemoved
		return &s
	case deployer.Unavailable:
		return nil
	}

	return nil
}

// getClusterHashAndStatus returns the hash of the EventTrigger that was deployed/evaluated in a given
// Cluster (if ever deployed/evaluated)
func (r *EventTriggerReconciler) getClusterHashAndStatus(resource *v1beta1.EventTrigger,
	cluster *corev1.ObjectReference) ([]byte, *libsveltosv1beta1.SveltosFeatureStatus) {

	for i := range resource.Status.ClusterInfo {
		clusterInfo := &resource.Status.ClusterInfo[i]
		if isClusterInfoForCluster(clusterInfo, cluster.Namespace, cluster.Name, clusterproxy.GetClusterType(cluster)) {
			return clusterInfo.Hash, &clusterInfo.Status
		}
	}

	return nil, nil
}

// isPaused returns true if Sveltos/CAPI Cluster is paused or EventTrigger has paused annotation.
func (r *EventTriggerReconciler) isPaused(ctx context.Context, cluster *corev1.ObjectReference,
	resource *v1beta1.EventTrigger) (bool, error) {

	isClusterPaused, err := clusterproxy.IsClusterPaused(ctx, r.Client, cluster.Namespace, cluster.Name,
		clusterproxy.GetClusterType(cluster))

	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	if isClusterPaused {
		return true, nil
	}

	return annotations.HasPaused(resource), nil
}

// canProceed returns true if cluster is ready to be programmed and it is not paused.
func (r *EventTriggerReconciler) canProceed(ctx context.Context, eScope *scope.EventTriggerScope,
	cluster *corev1.ObjectReference, logger logr.Logger) (bool, error) {

	logger = logger.WithValues("cluster", fmt.Sprintf("%s:%s/%s", cluster.Kind, cluster.Namespace, cluster.Name))

	paused, err := r.isPaused(ctx, cluster, eScope.EventTrigger)
	if err != nil {
		return false, err
	}

	if paused {
		logger.V(logs.LogDebug).Info("Cluster/EventTrigger is paused")
		return false, nil
	}

	ready, err := clusterproxy.IsClusterReadyToBeConfigured(ctx, r.Client, cluster, eScope.Logger)
	if err != nil {
		return false, err
	}

	if !ready {
		logger.V(logs.LogInfo).Info("Cluster is not ready yet")
		return false, nil
	}

	return true, nil
}

// isClusterStillMatching returns true if cluster is still matching by looking at EventBasedAddon
// Status MatchingClusterRefs
func isClusterStillMatching(eScope *scope.EventTriggerScope, cluster *corev1.ObjectReference) bool {
	for i := range eScope.EventTrigger.Status.MatchingClusterRefs {
		matchingCluster := &eScope.EventTrigger.Status.MatchingClusterRefs[i]
		if reflect.DeepEqual(*matchingCluster, *cluster) {
			return true
		}
	}
	return false
}

// isClusterConditionForCluster returns true if the ClusterCondition is for the cluster clusterType, clusterNamespace,
// clusterName
func isClusterInfoForCluster(clusterInfo *libsveltosv1beta1.ClusterInfo, clusterNamespace, clusterName string,
	clusterType libsveltosv1beta1.ClusterType) bool {

	return clusterInfo.Cluster.Namespace == clusterNamespace &&
		clusterInfo.Cluster.Name == clusterName &&
		clusterproxy.GetClusterType(&clusterInfo.Cluster) == clusterType
}

func removeClusterInfoEntry(ctx context.Context, c client.Client,
	clusterNamespace, clusterName string, clusterType libsveltosv1beta1.ClusterType,
	resource *v1beta1.EventTrigger, logger logr.Logger) error {

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		currentResource := &v1beta1.EventTrigger{}
		err := c.Get(ctx, types.NamespacedName{Name: resource.Name}, currentResource)
		if err != nil {
			return err
		}

		for i := range currentResource.Status.ClusterInfo {
			cc := &currentResource.Status.ClusterInfo[i]
			if isClusterInfoForCluster(cc, clusterNamespace, clusterName, clusterType) {
				currentResource.Status.ClusterInfo = remove(currentResource.Status.ClusterInfo, i)
				return c.Status().Update(context.TODO(), currentResource)
			}
		}

		return nil
	})

	return err
}

func remove(s []libsveltosv1beta1.ClusterInfo, i int) []libsveltosv1beta1.ClusterInfo {
	s[i] = s[len(s)-1]
	return s[:len(s)-1]
}

// deployEventSource deploys (creates or updates) referenced EventSource.
func deployEventSource(ctx context.Context, c client.Client,
	clusterNamespace, clusterName string, clusterType libsveltosv1beta1.ClusterType,
	resource *v1beta1.EventTrigger, logger logr.Logger) error {

	currentReferenced, err := fetchEventSource(ctx, c, clusterNamespace, clusterName, resource.Spec.EventSourceName,
		clusterType, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to collect EventSource %s: %v",
			resource.Spec.EventSourceName, err))
		return err
	}
	if currentReferenced == nil {
		logger.V(logs.LogInfo).Info("EventSource not found")
		return nil
	}

	var remoteClient client.Client
	remoteClient, err = clusterproxy.GetKubernetesClient(ctx, c, clusterNamespace, clusterName,
		"", "", clusterType, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get managed cluster client: %v", err))
		return err
	}

	// classifier installs sveltos-agent and CRDs it needs, including
	// EventSource and EventReport CRDs.

	err = createOrUpdateEventSource(ctx, remoteClient, resource, currentReferenced, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to create/update HealthCheck: %v", err))
		return err
	}

	return nil
}

func createOrUpdateEventSource(ctx context.Context, remoteClient client.Client, resource *v1beta1.EventTrigger,
	eventSource *libsveltosv1beta1.EventSource, logger logr.Logger) error {

	logger = logger.WithValues("eventSource", eventSource.Name)

	currentEventSource := &libsveltosv1beta1.EventSource{}
	err := remoteClient.Get(context.TODO(), types.NamespacedName{Name: eventSource.Name}, currentEventSource)
	if err == nil {
		logger.V(logs.LogDebug).Info("updating eventSource")
		currentEventSource.Spec = eventSource.Spec
		// Copy labels. If admin-label is set, sveltos-agent will impersonate
		// ServiceAccount representing the tenant admin when fetching resources
		currentEventSource.Labels = eventSource.Labels
		currentEventSource.Annotations = map[string]string{
			libsveltosv1beta1.DeployedBySveltosAnnotation: "true",
		}
		deployer.AddOwnerReference(currentEventSource, resource)
		return remoteClient.Update(ctx, currentEventSource)
	}

	currentEventSource.Name = eventSource.Name
	currentEventSource.Spec = eventSource.Spec
	// Copy labels. If admin-label is set, sveltos-agent will impersonate
	// ServiceAccount representing the tenant admin when fetching resources
	currentEventSource.Labels = eventSource.Labels
	deployer.AddOwnerReference(currentEventSource, resource)

	logger.V(logs.LogDebug).Info("creating eventSource")
	return remoteClient.Create(ctx, currentEventSource)
}

// removeStaleEventReports removes stale EventReports from the management cluster.
func removeStaleEventReports(ctx context.Context, c client.Client,
	clusterNamespace, clusterName, eventSourceName string,
	clusterType libsveltosv1beta1.ClusterType, logger logr.Logger) error {

	listOptions := []client.ListOption{
		client.InNamespace(clusterNamespace),
		client.MatchingLabels{
			libsveltosv1beta1.EventReportClusterNameLabel: clusterName,
			libsveltosv1beta1.EventReportClusterTypeLabel: strings.ToLower(string(clusterType)),
			libsveltosv1beta1.EventSourceNameLabel:        eventSourceName,
		},
	}

	eventReportList := &libsveltosv1beta1.EventReportList{}
	err := c.List(ctx, eventReportList, listOptions...)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to list EventReports. Err: %v", err))
		return err
	}

	for i := range eventReportList.Items {
		err = c.Delete(ctx, &eventReportList.Items[i])
		if err != nil {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to delete EventReport. Err: %v", err))
			return err
		}
	}

	return nil
}

// removeStaleEventSources removes stale EventSources.
// - If EventTrigger is deleted, EventTrigger will be removed as OwnerReference from any
// EventSource instance;
// - If EventTrigger is still existing, EventTrigger will be removed as OwnerReference from any
// EventSource instance it used to referenced and it is not referencing anymore.
// An EventSource with zero OwnerReference will be deleted from managed cluster.
func removeStaleEventSources(ctx context.Context, c client.Client,
	clusterNamespace, clusterName string, clusterType libsveltosv1beta1.ClusterType,
	resource *v1beta1.EventTrigger, removeAll bool, logger logr.Logger) error {

	remoteClient, err := clusterproxy.GetKubernetesClient(ctx, c, clusterNamespace, clusterName,
		"", "", clusterType, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get managed cluster client: %v", err))
		return err
	}

	eventSources := &libsveltosv1beta1.EventSourceList{}
	err = remoteClient.List(ctx, eventSources)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get list eventSources: %v", err))
		return err
	}

	for i := range eventSources.Items {
		es := &eventSources.Items[i]
		l := logger.WithValues("eventsource", es.Name)

		// removeAll indicates all EventSources deployed by this EventTrigger on this cluster
		// need to be removed (cluster is no longer a match)
		if !removeAll && resource.DeletionTimestamp.IsZero() &&
			es.Name == resource.Spec.EventSourceName {
			// eventTrigger still exists and eventSource is still referenced
			continue
		}

		if !util.IsOwnedByObject(es, resource) {
			continue
		}

		l.V(logs.LogDebug).Info("removing OwnerReference")
		deployer.RemoveOwnerReference(es, resource)

		if len(es.GetOwnerReferences()) != 0 {
			l.V(logs.LogDebug).Info("updating")
			// Other EventTrigger are still deploying this very same policy
			err = remoteClient.Update(ctx, es)
			if err != nil {
				logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get update EventSource: %v", err))
				return err
			}
			continue
		}

		// Since EventSource is about to be removed from the managed cluster, removes all
		// EventReports pulled from this managed cluster because of this EventSource
		err = removeStaleEventReports(ctx, c, clusterNamespace, clusterName, es.Name, clusterType, logger)
		if err != nil {
			return nil
		}

		l.V(logs.LogDebug).Info("deleting EventSource from managed cluster")
		err = remoteClient.Delete(ctx, es)
		if err != nil {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get delete EventSource: %v", err))
			return err
		}
	}

	return nil
}

// When instantiating one ClusterProfile for all resources those values are available.
// MatchingResources is always available. Resources is available only if EventSource.Spec.CollectResource is
// set to true (otherwise resources matching an EventSource won't be sent to management cluster)
type currentObjects struct {
	MatchingResources []corev1.ObjectReference
	Resources         []map[string]interface{}
	Cluster           map[string]interface{}
}

// When instantiating one ClusterProfile per resource those values are available.
// MatchingResource is always available. Resource is available only if EventSource.Spec.CollectResource is
// set to true (otherwise resources matching an EventSource won't be sent to management cluster)
type currentObject struct {
	MatchingResource corev1.ObjectReference
	Resource         map[string]interface{}
	Cluster          map[string]interface{}
}

// updateClusterProfiles creates/updates ClusterProfile(s).
// One or more clusterProfiles will be created/updated depending on eventTrigger.Spec.OneForEvent flag.
// ClusterProfile(s) content will have:
// - ClusterRef set to reference passed in cluster;
// - HelmCharts instantiated from EventTrigger.Spec.HelmCharts using values from resources, collected
// from the managed cluster, matching the EventSource referenced by EventTrigger
func updateClusterProfiles(ctx context.Context, c client.Client, clusterNamespace, clusterName string,
	clusterType libsveltosv1beta1.ClusterType, eventTrigger *v1beta1.EventTrigger, er *libsveltosv1beta1.EventReport,
	logger logr.Logger) error {

	// If no resource is currently matching, clear all
	if !er.DeletionTimestamp.IsZero() || len(er.Spec.MatchingResources) == 0 {
		return removeInstantiatedResources(ctx, c, clusterNamespace, clusterName, clusterType,
			eventTrigger, er, nil, logger)
	}

	var err error
	var clusterProfiles []*configv1beta1.ClusterProfile
	if eventTrigger.Spec.OneForEvent {
		clusterProfiles, err = instantiateOneClusterProfilePerResource(ctx, c, clusterNamespace, clusterName,
			clusterType, eventTrigger, er, logger)
		if err != nil {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to create one clusterProfile instance per matching resource: %v",
				err))
			return err
		}
	} else {
		clusterProfiles, err = instantiateOneClusterProfilePerAllResource(ctx, c, clusterNamespace, clusterName,
			clusterType, eventTrigger, er, logger)
		if err != nil {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to create one clusterProfile instance per matching resource: %v",
				err))
			return err
		}
	}

	// Remove stale ClusterProfiles/ConfigMaps/Secrets, i.e, resources previously created by this EventTrigger
	// instance for this cluster but currently not needed anymore
	return removeInstantiatedResources(ctx, c, clusterNamespace, clusterName, clusterType, eventTrigger, er,
		clusterProfiles, logger)
}

// instantiateOneClusterProfilePerResource instantiate a ClusterProfile for each resource currently matching the referenced
// EventSource (result is taken from EventReport).
// When instantiating:
// - "MatchingResource" references a corev1.ObjectReference representing the resource (always available)
// - "Resource" references an unstructured.Unstructured referencing the resource (available only if EventSource.Spec.CollectResources
// is set to true)
func instantiateOneClusterProfilePerResource(ctx context.Context, c client.Client, clusterNamespace, clusterName string,
	clusterType libsveltosv1beta1.ClusterType, eventTrigger *v1beta1.EventTrigger,
	eventReport *libsveltosv1beta1.EventReport, logger logr.Logger) ([]*configv1beta1.ClusterProfile, error) {

	clusterProfiles := make([]*configv1beta1.ClusterProfile, 0)
	resources, err := getResources(eventReport, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get matching resources %v", err))
		return nil, err
	}

	if len(resources) == 0 {
		for i := range eventReport.Spec.MatchingResources {
			var clusterProfile *configv1beta1.ClusterProfile
			clusterProfile, err = instantiateClusterProfileForResource(ctx, c, clusterNamespace, clusterName,
				clusterType, eventTrigger, eventReport, &eventReport.Spec.MatchingResources[i], nil, logger)
			if err != nil {
				return nil, err
			}
			clusterProfiles = append(clusterProfiles, clusterProfile)
		}
		return clusterProfiles, nil
	}

	for i := range resources {
		r := &resources[i]
		var clusterProfile *configv1beta1.ClusterProfile
		matchingResource := corev1.ObjectReference{
			APIVersion: r.GetAPIVersion(), Kind: r.GetKind(),
			Namespace: r.GetNamespace(), Name: r.GetName(),
		}

		clusterProfile, err = instantiateClusterProfileForResource(ctx, c, clusterNamespace, clusterName,
			clusterType, eventTrigger, eventReport, &matchingResource, r, logger)
		if err != nil {
			return nil, err
		}
		clusterProfiles = append(clusterProfiles, clusterProfile)
	}

	return clusterProfiles, nil
}

// instantiateClusterProfileForResource creates one ClusterProfile by:
// - setting Spec.ClusterRef reference passed in cluster clusterNamespace/clusterName/ClusterType
// - instantiating eventTrigger.Spec.HelmCharts with passed in resource (one of the resource matching referenced EventSource)
// and copying this value to ClusterProfile.Spec.HelmCharts
// - instantiating eventTrigger.Spec.PolicyRefs with passed in resource (one of the resource matching referenced EventSource)
// in new ConfigMaps/Secrets and have ClusterProfile.Spec.PolicyRefs reference those;
// - labels are added to ClusterProfile to easily fetch all ClusterProfiles created by a given EventTrigger
func instantiateClusterProfileForResource(ctx context.Context, c client.Client, clusterNamespace, clusterName string,
	clusterType libsveltosv1beta1.ClusterType, eventTrigger *v1beta1.EventTrigger, er *libsveltosv1beta1.EventReport,
	matchingResource *corev1.ObjectReference, resource *unstructured.Unstructured, logger logr.Logger,
) (*configv1beta1.ClusterProfile, error) {

	object, err := prepareCurrentObject(ctx, c, clusterNamespace, clusterName, clusterType, resource,
		matchingResource, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to prepare currentObject %v", err))
		return nil, err
	}

	labels := getInstantiatedObjectLabels(clusterNamespace, clusterName, eventTrigger.Name,
		er, clusterType)

	tmpLabels := getInstantiatedObjectLabelsForResource(matchingResource.Namespace, matchingResource.Name)
	for k, v := range tmpLabels {
		labels[k] = v
	}

	clusterProfileName, createClusterProfile, err := getClusterProfileName(ctx, c, labels)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get ClusterProfile name: %v", err))
		return nil, err
	}

	// If EventTrigger was created by tenant admin, copy label over to created ClusterProfile
	if eventTrigger.Labels != nil {
		if serviceAccountName, ok := eventTrigger.Labels[libsveltosv1beta1.ServiceAccountNameLabel]; ok {
			labels[libsveltosv1beta1.ServiceAccountNameLabel] = serviceAccountName
		}
		if serviceAccountNamespace, ok := eventTrigger.Labels[libsveltosv1beta1.ServiceAccountNamespaceLabel]; ok {
			labels[libsveltosv1beta1.ServiceAccountNamespaceLabel] = serviceAccountNamespace
		}
	}

	clusterProfile := getNonInstantiatedClusterProfile(eventTrigger, clusterProfileName, labels)

	templateName := getTemplateName(clusterNamespace, clusterName, eventTrigger.Name)
	templateResourceRefs, err := instantiateTemplateResourceRefs(templateName, object.Cluster, object,
		eventTrigger.Spec.TemplateResourceRefs)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to instantiate TemplateResourceRefs: %v", err))
		return nil, err
	}
	clusterProfile.Spec.TemplateResourceRefs = templateResourceRefs

	if reflect.DeepEqual(eventTrigger.Spec.DestinationClusterSelector, libsveltosv1beta1.Selector{}) {
		clusterProfile.Spec.ClusterRefs = []corev1.ObjectReference{*getClusterRef(clusterNamespace, clusterName, clusterType)}
		clusterProfile.Spec.ClusterSelector = libsveltosv1beta1.Selector{}
	} else {
		clusterProfile.Spec.ClusterRefs = nil
		clusterProfile.Spec.ClusterSelector = eventTrigger.Spec.DestinationClusterSelector
	}

	instantiateHelmChartsWithResource, err := instantiateHelmChartsWithResource(ctx, c, clusterNamespace, templateName,
		eventTrigger.Spec.HelmCharts, object, labels, logger)
	if err != nil {
		return nil, err
	}
	clusterProfile.Spec.HelmCharts = instantiateHelmChartsWithResource

	instantiateKustomizeRefsWithResource, err := instantiateKustomizationRefsWithResource(ctx, c, clusterNamespace,
		templateName, eventTrigger.Spec.KustomizationRefs, object, labels, logger)
	if err != nil {
		return nil, err
	}
	clusterProfile.Spec.KustomizationRefs = instantiateKustomizeRefsWithResource

	clusterRef := getClusterRef(clusterNamespace, clusterName, clusterType)
	localPolicyRef, remotePolicyRef, err := instantiateReferencedPolicies(ctx, c, templateName,
		eventTrigger, clusterRef, object, labels, logger)
	if err != nil {
		return nil, err
	}
	clusterProfile.Spec.PolicyRefs = getClusterProfilePolicyRefs(localPolicyRef, remotePolicyRef)

	if createClusterProfile {
		return clusterProfile, c.Create(ctx, clusterProfile)
	}

	return clusterProfile, updateClusterProfileSpec(ctx, c, clusterProfile, logger)
}

// instantiateOneClusterProfilePerAllResource creates one ClusterProfile by:
// - setting Spec.ClusterRef reference passed in cluster clusterNamespace/clusterName/ClusterType
// - instantiating eventTrigger.Spec.HelmCharts with passed in resource (one of the resource matching referenced EventSource)
// and copying this value to ClusterProfile.Spec.HelmCharts
// - instantiating eventTrigger.Spec.PolicyRefs with passed in resource (one of the resource matching referenced EventSource)
// in new ConfigMaps/Secrets and have ClusterProfile.Spec.PolicyRefs reference those;
// - labels are added to ClusterProfile to easily fetch all ClusterProfiles created by a given EvnteTrigger
func instantiateOneClusterProfilePerAllResource(ctx context.Context, c client.Client, clusterNamespace, clusterName string,
	clusterType libsveltosv1beta1.ClusterType, eventTrigger *v1beta1.EventTrigger,
	eventReport *libsveltosv1beta1.EventReport, logger logr.Logger) ([]*configv1beta1.ClusterProfile, error) {

	resources, err := getResources(eventReport, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get matching resources %v", err))
		return nil, err
	}

	objects, err := prepareCurrentObjects(ctx, c, clusterNamespace, clusterName, clusterType,
		eventReport, resources, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to prepare currentObjects %v", err))
		return nil, err
	}

	labels := getInstantiatedObjectLabels(clusterNamespace, clusterName, eventTrigger.Name,
		eventReport, clusterType)

	clusterProfileName, createClusterProfile, err := getClusterProfileName(ctx, c, labels)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get ClusterProfile name: %v", err))
		return nil, err
	}

	// If EventTrigger was created by tenant admin, copy label over to created ClusterProfile
	if eventTrigger.Labels != nil {
		if serviceAccountName, ok := eventTrigger.Labels[libsveltosv1beta1.ServiceAccountNameLabel]; ok {
			labels[libsveltosv1beta1.ServiceAccountNameLabel] = serviceAccountName
		}
		if serviceAccountNamespace, ok := eventTrigger.Labels[libsveltosv1beta1.ServiceAccountNamespaceLabel]; ok {
			labels[libsveltosv1beta1.ServiceAccountNamespaceLabel] = serviceAccountNamespace
		}
	}

	clusterProfile := getNonInstantiatedClusterProfile(eventTrigger, clusterProfileName, labels)

	templateName := getTemplateName(clusterNamespace, clusterName, eventTrigger.Name)
	templateResourceRefs, err := instantiateTemplateResourceRefs(templateName, objects.Cluster, objects,
		eventTrigger.Spec.TemplateResourceRefs)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to instantiate TemplateResourceRefs: %v", err))
		return nil, err
	}
	clusterProfile.Spec.TemplateResourceRefs = templateResourceRefs

	if reflect.DeepEqual(eventTrigger.Spec.DestinationClusterSelector, libsveltosv1beta1.Selector{}) {
		clusterProfile.Spec.ClusterRefs = []corev1.ObjectReference{*getClusterRef(clusterNamespace, clusterName, clusterType)}
		clusterProfile.Spec.ClusterSelector = libsveltosv1beta1.Selector{}
	} else {
		clusterProfile.Spec.ClusterRefs = nil
		clusterProfile.Spec.ClusterSelector = eventTrigger.Spec.DestinationClusterSelector
	}

	instantiateHelmChartsWithResources, err := instantiateHelmChartsWithAllResources(ctx, c, clusterNamespace, templateName,
		eventTrigger.Spec.HelmCharts, objects, labels, logger)
	if err != nil {
		return nil, err
	}
	clusterProfile.Spec.HelmCharts = instantiateHelmChartsWithResources

	instantiateKustomizeRefsWithResource, err := instantiateKustomizationRefsWithAllResources(ctx, c, clusterNamespace,
		templateName, eventTrigger.Spec.KustomizationRefs, objects, labels, logger)
	if err != nil {
		return nil, err
	}
	clusterProfile.Spec.KustomizationRefs = instantiateKustomizeRefsWithResource

	clusterRef := getClusterRef(clusterNamespace, clusterName, clusterType)
	localPolicyRef, remotePolicyRef, err := instantiateReferencedPolicies(ctx, c, templateName,
		eventTrigger, clusterRef, objects, labels, logger)
	if err != nil {
		return nil, err
	}
	clusterProfile.Spec.PolicyRefs = getClusterProfilePolicyRefs(localPolicyRef, remotePolicyRef)

	if createClusterProfile {
		return []*configv1beta1.ClusterProfile{clusterProfile}, c.Create(ctx, clusterProfile)
	}

	return []*configv1beta1.ClusterProfile{clusterProfile}, updateClusterProfileSpec(ctx, c, clusterProfile, logger)
}

func getClusterProfilePolicyRefs(localPolicyRef, remotePolicyRef *libsveltosset.Set) []configv1beta1.PolicyRef {
	result := make([]configv1beta1.PolicyRef, localPolicyRef.Len()+remotePolicyRef.Len())

	secret := "Secret"

	// Add local policyRef
	items := localPolicyRef.Items()
	for i := range items {
		kind := libsveltosv1beta1.ConfigMapReferencedResourceKind
		if items[i].Kind == secret {
			kind = libsveltosv1beta1.SecretReferencedResourceKind
		}
		result[i] = configv1beta1.PolicyRef{
			DeploymentType: configv1beta1.DeploymentTypeLocal,
			Namespace:      items[i].Namespace,
			Name:           items[i].Name,
			Kind:           string(kind),
		}
	}

	numOfPolicyItems := localPolicyRef.Len()
	// Add remote policyRef
	items = remotePolicyRef.Items()
	for i := range items {
		kind := libsveltosv1beta1.ConfigMapReferencedResourceKind
		if items[i].Kind == secret {
			kind = libsveltosv1beta1.SecretReferencedResourceKind
		}
		result[numOfPolicyItems+i] = configv1beta1.PolicyRef{
			DeploymentType: configv1beta1.DeploymentTypeRemote,
			Namespace:      items[i].Namespace,
			Name:           items[i].Name,
			Kind:           string(kind),
		}
	}

	return result
}

func updateClusterProfileSpec(ctx context.Context, c client.Client, clusterProfile *configv1beta1.ClusterProfile,
	logger logr.Logger) error {

	currentClusterProfile := &configv1beta1.ClusterProfile{}

	err := c.Get(ctx, types.NamespacedName{Name: clusterProfile.Name}, currentClusterProfile)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get ClusterProfile: %v", err))
		return err
	}

	currentClusterProfile.Spec = clusterProfile.Spec

	return c.Update(ctx, currentClusterProfile)
}

// getResources returns a slice of unstructured.Unstructured by processing eventReport.Spec.Resources field
func getResources(eventReport *libsveltosv1beta1.EventReport, logger logr.Logger) ([]unstructured.Unstructured, error) {
	elements := strings.Split(string(eventReport.Spec.Resources), "---")
	result := make([]unstructured.Unstructured, 0)
	for i := range elements {
		if elements[i] == "" {
			continue
		}

		var err error
		var policy *unstructured.Unstructured
		policy, err = libsveltosutils.GetUnstructured([]byte(elements[i]))
		if err != nil {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get policy from Data %.100s", elements[i]))
			return nil, err
		}

		result = append(result, *policy)
	}

	return result, nil
}

func instantiateSection(templateName string, toBeInstantiated []byte, data any,
	logger logr.Logger) ([]byte, error) {

	tmpl, err := template.New(templateName).Option("missingkey=error").Funcs(
		funcmap.SveltosFuncMap()).Parse(string(toBeInstantiated))
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to parse template: %v", err))
		return nil, err
	}

	var buffer bytes.Buffer
	if err = tmpl.Execute(&buffer, data); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to execute template: %v", err))
		return nil, err
	}

	return buffer.Bytes(), nil
}

func instantiateHelmCharts(ctx context.Context, c client.Client, clusterNamespace, templateName string,
	helmCharts []configv1beta1.HelmChart, data any, labels map[string]string, logger logr.Logger,
) ([]configv1beta1.HelmChart, error) {

	helmChartJson, err := json.Marshal(helmCharts)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshel helmCharts: %v", err))
		return nil, err
	}

	instantiatedData, err := instantiateSection(templateName, helmChartJson, data, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to execute template: %v", err))
		return nil, err
	}

	instantiatedHelmCharts := make([]configv1beta1.HelmChart, 0)
	err = json.Unmarshal(instantiatedData, &instantiatedHelmCharts)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to unmarshal helmCharts: %v", err))
		return nil, err
	}

	for i := range instantiatedHelmCharts {
		err = instantiateValuesFrom(ctx, c, instantiatedHelmCharts[i].ValuesFrom, clusterNamespace, templateName,
			data, labels, logger)
		if err != nil {
			return nil, err
		}
	}

	return instantiatedHelmCharts, nil
}

func instantiateKustomizationRefs(ctx context.Context, c client.Client, clusterNamespace, templateName string,
	kustomizationRefs []configv1beta1.KustomizationRef, data any, labels map[string]string, logger logr.Logger,
) ([]configv1beta1.KustomizationRef, error) {

	kustomizationRefsJson, err := json.Marshal(kustomizationRefs)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshel kustomizationRefs: %v", err))
		return nil, err
	}

	instantiatedData, err := instantiateSection(templateName, kustomizationRefsJson, data, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to execute template: %v", err))
		return nil, err
	}

	instantiatedKustomizationRefs := make([]configv1beta1.KustomizationRef, 0)
	err = json.Unmarshal(instantiatedData, &instantiatedKustomizationRefs)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to unmarshal kustomizationRefs: %v", err))
		return nil, err
	}

	for i := range instantiatedKustomizationRefs {
		err = instantiateValuesFrom(ctx, c, instantiatedKustomizationRefs[i].ValuesFrom, clusterNamespace,
			templateName, data, labels, logger)
		if err != nil {
			return nil, err
		}
	}

	return instantiatedKustomizationRefs, nil
}

func instantiateValuesFrom(ctx context.Context, c client.Client, valuesFrom []configv1beta1.ValueFrom,
	clusterNamespace, templateName string, data any, labels map[string]string, logger logr.Logger) error {

	for i := range valuesFrom {
		ref := &valuesFrom[i]

		namespace := libsveltostemplate.GetReferenceResourceNamespace(clusterNamespace, ref.Namespace)

		var err error
		var resource client.Object
		if ref.Kind == string(libsveltosv1beta1.ConfigMapReferencedResourceKind) {
			resource, err = getConfigMap(ctx, c, types.NamespacedName{Namespace: namespace, Name: ref.Name})
		} else {
			resource, err = getSecret(ctx, c, types.NamespacedName{Namespace: namespace, Name: ref.Name})
		}

		if err != nil {
			if apierrors.IsNotFound(err) {
				// referenced ConfigMap/Secret does not exist. Assume is not intended to be a template.
				// So there is no need to instantiate a new one. Generated ClusterProfile can directly
				// reference this one
				continue
			}
			return err
		}

		var info *types.NamespacedName
		if _, ok := resource.GetAnnotations()[libsveltosv1beta1.PolicyTemplateAnnotation]; !ok {
			// referenced ConfigMap/Secret is not a template. So there is no
			// need to instantiate a new one. Generated ClusterProfile can directly
			// reference this one
			info = &types.NamespacedName{Namespace: resource.GetNamespace(), Name: resource.GetName()}
		} else {
			info, err = instantiateReferencedPolicy(ctx, c, resource, templateName, data, labels, logger)
		}

		if err != nil {
			msg := fmt.Sprintf("failed to instantiate content for ValuesFrom: %s/%s",
				resource.GetNamespace(), resource.GetName())
			logger.V(logs.LogInfo).Info(fmt.Sprintf("%s. Error: %v", msg, err))
			return errors.Wrapf(err, msg)
		}

		ref.Name = info.Name
		ref.Namespace = info.Namespace
	}

	return nil
}

func instantiateDataSection(templateName string, content map[string]string, data any,
	logger logr.Logger) (map[string]string, error) {

	contentJson, err := json.Marshal(content)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to marshal content: %v", err))
		return nil, err
	}

	tmpl, err := template.New(templateName).Option("missingkey=error").Funcs(
		funcmap.SveltosFuncMap()).Parse(string(contentJson))
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to parse content: %v", err))
		return nil, err
	}

	var buffer bytes.Buffer
	if err = tmpl.Execute(&buffer, data); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to execute content: %v", err))
		return nil, err
	}

	instantiatedContent := make(map[string]string)
	err = json.Unmarshal(buffer.Bytes(), &instantiatedContent)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to unmarshal content: %v", err))
		return nil, err
	}

	return instantiatedContent, nil
}

func instantiateTemplateResourceRefs(templateName string, clusterContent map[string]interface{}, data any,
	templateResourceRefs []configv1beta1.TemplateResourceRef) ([]configv1beta1.TemplateResourceRef, error) {

	var uCluster unstructured.Unstructured
	uCluster.SetUnstructuredContent(clusterContent)

	instantiated := make([]configv1beta1.TemplateResourceRef, len(templateResourceRefs))
	for i := range templateResourceRefs {
		namespace := libsveltostemplate.GetReferenceResourceNamespace(uCluster.GetNamespace(),
			templateResourceRefs[i].Resource.Namespace)

		tmpl, err := template.New(templateName).Option("missingkey=error").Funcs(
			funcmap.SveltosFuncMap()).Parse(templateResourceRefs[i].Resource.Name)
		if err != nil {
			return nil, err
		}

		var buffer bytes.Buffer
		err = tmpl.Execute(&buffer, data)
		if err != nil {
			return nil, err
		}

		instantiated[i] = templateResourceRefs[i]
		instantiated[i].Resource.Namespace = namespace
		instantiated[i].Resource.Name = buffer.String()
	}

	return instantiated, nil
}

// instantiateHelmChartsWithResource instantiate eventTrigger.Spec.HelmCharts using information from passed in object
// which represents one of the resource matching referenced EventSource in the managed cluster.
func instantiateHelmChartsWithResource(ctx context.Context, c client.Client, clusterNamespace, templateName string,
	helmCharts []configv1beta1.HelmChart, object *currentObject, labels map[string]string, logger logr.Logger,
) ([]configv1beta1.HelmChart, error) {

	return instantiateHelmCharts(ctx, c, clusterNamespace, templateName, helmCharts, object, labels, logger)
}

// instantiateHelmChartsWithAllResources instantiate eventTrigger.Spec.HelmCharts using information from passed in objects
// which represent all of the resources matching referenced EventSource in the managed cluster.
func instantiateHelmChartsWithAllResources(ctx context.Context, c client.Client, clusterNamespace, templateName string,
	helmCharts []configv1beta1.HelmChart, objects *currentObjects, labels map[string]string, logger logr.Logger,
) ([]configv1beta1.HelmChart, error) {

	return instantiateHelmCharts(ctx, c, clusterNamespace, templateName, helmCharts, objects, labels, logger)
}

// instantiateKustomizationRefsWithResource instantiate eventTrigger.Spec.KustomizationRefs using information from passed
// in object which represents one of the resource matching referenced EventSource in the managed cluster.
func instantiateKustomizationRefsWithResource(ctx context.Context, c client.Client, clusterNamespace, templateName string,
	kustomizationRefs []configv1beta1.KustomizationRef, object *currentObject, labels map[string]string, logger logr.Logger,
) ([]configv1beta1.KustomizationRef, error) {

	return instantiateKustomizationRefs(ctx, c, clusterNamespace, templateName, kustomizationRefs, object, labels, logger)
}

// instantiateKustomizationRefsWithAllResources instantiate eventTrigger.Spec.KustomizationRefs using information from passed
// in objects which represent all of the resources matching referenced EventSource in the managed cluster.
func instantiateKustomizationRefsWithAllResources(ctx context.Context, c client.Client, clusterNamespace, templateName string,
	kustomizationRefs []configv1beta1.KustomizationRef, objects *currentObjects, labels map[string]string, logger logr.Logger,
) ([]configv1beta1.KustomizationRef, error) {

	return instantiateKustomizationRefs(ctx, c, clusterNamespace, templateName, kustomizationRefs, objects, labels, logger)
}

// instantiateReferencedPolicies instantiate eventTrigger.Spec.PolicyRefs using information from passed in objects
// which represent all of the resources matching referenced EventSource in the managed cluster.
func instantiateReferencedPolicies(ctx context.Context, c client.Client, templateName string,
	eventTrigger *v1beta1.EventTrigger, cluster *corev1.ObjectReference, objects any,
	labels map[string]string, logger logr.Logger) (localSet, remoteSet *libsveltosset.Set, err error) {

	// fetches all referenced ConfigMaps/Secrets
	var local []client.Object
	var remote []client.Object
	local, remote, err = fetchPolicyRefs(ctx, c, eventTrigger, cluster, objects, templateName, logger)
	if err != nil {
		return nil, nil, err
	}

	localSet, err = instantiateResources(ctx, c, templateName, local, objects, labels, logger)
	if err != nil {
		return nil, nil, err
	}
	remoteSet, err = instantiateResources(ctx, c, templateName, remote, objects, labels, logger)
	if err != nil {
		return nil, nil, err
	}

	return
}

func instantiateResources(ctx context.Context, c client.Client, templateName string, resources []client.Object,
	objects any, labels map[string]string, logger logr.Logger) (*libsveltosset.Set, error) {

	result := libsveltosset.Set{}

	// For each referenced resource, instantiate it using objects collected from managed cluster
	// and create/update corresponding ConfigMap/Secret in managemenent cluster
	for i := range resources {
		ref := resources[i]
		apiVersion, kind := ref.GetObjectKind().GroupVersionKind().ToAPIVersionAndKind()

		l := logger.WithValues("referencedResource", fmt.Sprintf("%s:%s/%s",
			ref.GetObjectKind().GroupVersionKind().Kind, ref.GetNamespace(), ref.GetName()))
		l.V(logs.LogDebug).Info("process referenced resource")
		var info *types.NamespacedName
		var err error
		if _, ok := ref.GetAnnotations()[libsveltosv1beta1.PolicyTemplateAnnotation]; !ok {
			// referenced ConfigMap/Secret is not a template. So there is no
			// need to instantiate a new one. Generated ClusterProfile can directly
			// reference this one
			info = &types.NamespacedName{Namespace: ref.GetNamespace(), Name: ref.GetName()}
		} else {
			info, err = instantiateReferencedPolicy(ctx, c, ref, templateName, objects, labels, logger)
		}

		if err != nil {
			return nil, err
		}

		result.Insert(&corev1.ObjectReference{APIVersion: apiVersion, Kind: kind,
			Namespace: info.Namespace, Name: info.Name})
	}

	return &result, nil
}

func instantiateReferencedPolicy(ctx context.Context, c client.Client, ref client.Object,
	templateName string, objects any, labels map[string]string, logger logr.Logger,
) (*types.NamespacedName, error) {

	l := logger.WithValues("referencedResource",
		fmt.Sprintf("%s:%s/%s", ref.GetObjectKind(), ref.GetNamespace(), ref.GetName()))

	content := getDataSection(ref)
	// If referenced resource is a template, assume it needs to be instantiated using
	// information from the resources in the managed cluster that generated the event.
	// Generate then a new ConfigMap/Secret. The autocreated ClusterProfile will reference
	// this new resource.

	instantiatedContent, err := instantiateDataSection(templateName, content, objects, l)
	if err != nil {
		l.V(logs.LogInfo).Info(fmt.Sprintf("failed to instantiated referenced resource content: %v", err))
		return nil, err
	}
	content = instantiatedContent

	// Resource name must depend on reference resource name as well. So add those labels.
	// If an EventTrigger is referencing N configMaps/Secrets, N equivalent referenced
	// resources must be created
	labels[referencedResourceNamespaceLabel] = ref.GetNamespace()
	labels[referencedResourceNameLabel] = ref.GetName()

	name, create, err := getResourceName(ctx, c, ref, labels)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get %s name: %v", ref.GetObjectKind(), err))
		return nil, err
	}

	if create {
		logger.V(logs.LogDebug).Info(fmt.Sprintf("create resource for %s %s:%s",
			ref.GetObjectKind().GroupVersionKind().Kind, ref.GetNamespace(), ref.GetName()))
		err = createResource(ctx, c, ref, name, labels, content)
		if err != nil {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to create resource: %v", err))
			return nil, err
		}
	} else {
		logger.V(logs.LogDebug).Info(fmt.Sprintf("update resource for %s %s:%s",
			ref.GetObjectKind().GroupVersionKind().Kind, ref.GetNamespace(), ref.GetName()))
		err = updateResource(ctx, c, ref, name, labels, content)
		if err != nil {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to update resource: %v", err))
			return nil, err
		}
	}

	return &types.NamespacedName{Namespace: ReportNamespace, Name: name}, nil
}

// createResource creates either a ConfigMap or a Secret based on ref type.
// Resource is created in the ReportNamespace.
// On the newly created resource, labels and Data are set
func createResource(ctx context.Context, c client.Client, ref client.Object, name string,
	labels, content map[string]string) error {

	switch ref.(type) {
	case *corev1.ConfigMap:
		return createConfigMap(ctx, c, ref, name, labels, content)
	case *corev1.Secret:
		return createSecret(ctx, c, ref, name, labels, content)
	default:
		panic(1) // only referenced resources are ConfigMap/Secret
	}
}

func createConfigMap(ctx context.Context, c client.Client, ref client.Object, name string,
	labels, content map[string]string) error {

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ReportNamespace,
			Labels:      labels,
			Annotations: ref.GetAnnotations(), //  libsveltosv1beta1.PolicyTemplateAnnotation might be set
		},
		Data: content,
	}

	return c.Create(ctx, cm)
}

func createSecret(ctx context.Context, c client.Client, ref client.Object, name string,
	labels, content map[string]string) error {

	data := make(map[string][]byte)
	for key, value := range content {
		data[key] = []byte(value)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ReportNamespace,
			Labels:      labels,
			Annotations: ref.GetAnnotations(), //  libsveltosv1beta1.PolicyTemplateAnnotation might be set
		},
		Data: data,
		Type: libsveltosv1beta1.ClusterProfileSecretType,
	}

	return c.Create(ctx, secret)
}

// updateResource updates either a ConfigMap or a Secret based on ref type.
// Resource is in the ReportNamespace.
// Resource's labels and Data are set
func updateResource(ctx context.Context, c client.Client, ref client.Object, name string,
	labels, content map[string]string) error {

	switch ref.(type) {
	case *corev1.ConfigMap:
		return updateConfigMap(ctx, c, name, labels, content)
	case *corev1.Secret:
		return updateSecret(ctx, c, name, labels, content)
	default:
		panic(1) // only referenced resources are ConfigMap/Secret
	}
}

func updateConfigMap(ctx context.Context, c client.Client, name string, labels, content map[string]string,
) error {

	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: ReportNamespace, Name: name}, cm)
	if err != nil {
		return err
	}

	cm.Labels = labels
	cm.Data = content

	return c.Update(ctx, cm)
}

func updateSecret(ctx context.Context, c client.Client, name string, labels, content map[string]string,
) error {

	data := make(map[string][]byte)
	for key, value := range content {
		data[key] = []byte(value)
	}

	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Namespace: ReportNamespace, Name: name}, secret)
	if err != nil {
		return err
	}

	secret.Labels = labels
	secret.Data = data
	secret.Type = libsveltosv1beta1.ClusterProfileSecretType

	return c.Update(ctx, secret)
}

func getDataSection(ref client.Object) map[string]string {
	switch v := ref.(type) {
	case *corev1.ConfigMap:
		return v.Data
	case *corev1.Secret:
		data := make(map[string]string)
		for key, value := range v.Data {
			data[key] = string(value)
		}
		return data
	default:
		panic(1) // only referenced resources are ConfigMap/Secret
	}
}

func getTemplateName(clusterNamespace, clusterName, requestorName string) string {
	return fmt.Sprintf("%s-%s-%s", clusterNamespace, clusterName, requestorName)
}

func getClusterRef(clusterNamespace, clusterName string,
	clusterType libsveltosv1beta1.ClusterType) *corev1.ObjectReference {

	ref := &corev1.ObjectReference{
		Namespace: clusterNamespace,
		Name:      clusterName,
	}

	if clusterType == libsveltosv1beta1.ClusterTypeSveltos {
		ref.APIVersion = libsveltosv1beta1.GroupVersion.String()
		ref.Kind = libsveltosv1beta1.SveltosClusterKind
	} else {
		ref.APIVersion = clusterv1.GroupVersion.String()
		ref.Kind = "Cluster"
	}

	return ref
}

// getClusterProfileName returns the name for a given ClusterProfile given the labels such ClusterProfile
// should have. It also returns whether the ClusterProfile must be created (if create a false, ClusterProfile
// should be simply updated). And an error if any occurs.
func getClusterProfileName(ctx context.Context, c client.Client, labels map[string]string,
) (name string, create bool, err error) {

	listOptions := []client.ListOption{
		client.MatchingLabels(labels),
	}

	clusterProfileList := &configv1beta1.ClusterProfileList{}
	err = c.List(ctx, clusterProfileList, listOptions...)
	if err != nil {
		return
	}

	objects := make([]client.Object, len(clusterProfileList.Items))
	for i := range clusterProfileList.Items {
		objects[i] = &clusterProfileList.Items[i]
	}

	return getInstantiatedObjectName(objects)
}

func getResourceName(ctx context.Context, c client.Client, ref client.Object,
	labels map[string]string) (name string, create bool, err error) {

	switch ref.(type) {
	case *corev1.ConfigMap:
		name, create, err = getConfigMapName(ctx, c, labels)
	case *corev1.Secret:
		name, create, err = getSecretName(ctx, c, labels)
	default:
		panic(1)
	}
	return
}

// getConfigMapName returns the name for a given ConfigMap given the labels such ConfigMap
// should have. It also returns whether the ConfigMap must be created (if create a false, ConfigMap
// should be simply updated). And an error if any occurs.
func getConfigMapName(ctx context.Context, c client.Client, labels map[string]string,
) (name string, create bool, err error) {

	listOptions := []client.ListOption{
		client.MatchingLabels(labels),
		client.InNamespace(ReportNamespace), // all instantianted ConfigMaps are in this namespace
	}

	configMapList := &corev1.ConfigMapList{}
	err = c.List(ctx, configMapList, listOptions...)
	if err != nil {
		return
	}

	objects := make([]client.Object, len(configMapList.Items))
	for i := range configMapList.Items {
		objects[i] = &configMapList.Items[i]
	}

	return getInstantiatedObjectName(objects)
}

// getSecretName returns the name for a given Secret given the labels such Secret
// should have. It also returns whether the Secret must be created (if create a false, Secret
// should be simply updated). And an error if any occurs.
func getSecretName(ctx context.Context, c client.Client, labels map[string]string,
) (name string, create bool, err error) {

	listOptions := []client.ListOption{
		client.MatchingLabels(labels),
		client.InNamespace(ReportNamespace), // all instantianted Secrets are in this namespace
	}

	secretList := &corev1.SecretList{}
	err = c.List(ctx, secretList, listOptions...)
	if err != nil {
		return
	}

	objects := make([]client.Object, len(secretList.Items))
	for i := range secretList.Items {
		objects[i] = &secretList.Items[i]
	}

	return getInstantiatedObjectName(objects)
}

func getInstantiatedObjectName(objects []client.Object) (name string, create bool, err error) {
	prefix := "sveltos-"
	switch len(objects) {
	case 0:
		// no cluster exist yet. Return random name.
		// If one clusterProfile with this name already exists,
		// a conflict will happen. On retry different name will
		// be picked
		const nameLength = 20
		name = prefix + util.RandomString(nameLength)
		create = true
		err = nil
	case 1:
		name = objects[0].GetName()
		create = false
		err = nil
	default:
		err = fmt.Errorf("more than one resource")
	}
	return name, create, err
}

// getInstantiatedObjectLabels returns the labels to add to a ClusterProfile created
// by an EventTrigger for a given cluster
func getInstantiatedObjectLabels(clusterNamespace, clusterName, eventTriggerName string,
	er *libsveltosv1beta1.EventReport, clusterType libsveltosv1beta1.ClusterType) map[string]string {

	labels := map[string]string{
		eventTriggerNameLabel: eventTriggerName,
		clusterNamespaceLabel: clusterNamespace,
		clusterNameLabel:      clusterName,
		clusterTypeLabel:      string(clusterType),
	}

	// When deleting all resources created by an EventTrigger, er will be nil
	if er != nil {
		labels[eventReportNameLabel] = er.Name
	}

	return labels
}

// getInstantiatedObjectLabelsForResource returns the label to add to a ClusterProfile created
// for a specific resource
func getInstantiatedObjectLabelsForResource(resourceNamespace, resourceName string) map[string]string {
	labels := map[string]string{
		"eventtrigger.lib.projectsveltos.io/resourcename": resourceName,
	}

	if resourceNamespace != "" {
		labels["eventtrigger.lib.projectsveltos.io/resourcenamespace"] = resourceNamespace
	}

	return labels
}

func removeInstantiatedResources(ctx context.Context, c client.Client, clusterNamespace, clusterName string,
	clusterType libsveltosv1beta1.ClusterType, eventTrigger *v1beta1.EventTrigger, er *libsveltosv1beta1.EventReport,
	clusterProfiles []*configv1beta1.ClusterProfile, logger logr.Logger) error {

	if err := removeClusterProfiles(ctx, c, clusterNamespace, clusterName, clusterType, eventTrigger, er,
		clusterProfiles, logger); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to remove stale clusterProfiles: %v", err))
		return err
	}

	policyRefs := make(map[libsveltosv1beta1.PolicyRef]bool) // ignore deploymentType
	for i := range clusterProfiles {
		cp := clusterProfiles[i]
		for j := range cp.Spec.PolicyRefs {
			policyRefs[libsveltosv1beta1.PolicyRef{
				Namespace: cp.Spec.PolicyRefs[j].Namespace,
				Name:      cp.Spec.PolicyRefs[j].Name,
				Kind:      cp.Spec.PolicyRefs[j].Kind,
			}] = true
		}
		policyRefs = appendHelmChartValuesFrom(policyRefs, cp.Spec.HelmCharts)
		policyRefs = appendKustomizationRefValuesFrom(policyRefs, cp.Spec.KustomizationRefs)
	}

	if err := removeConfigMaps(ctx, c, clusterNamespace, clusterName, clusterType, eventTrigger,
		er, policyRefs, logger); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to remove stale configMaps: %v", err))
		return err
	}

	if err := removeSecrets(ctx, c, clusterNamespace, clusterName, clusterType, eventTrigger,
		er, policyRefs, logger); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to remove stale secrets: %v", err))
		return err
	}

	return nil
}

func appendHelmChartValuesFrom(policyRefs map[libsveltosv1beta1.PolicyRef]bool, helmCharts []configv1beta1.HelmChart,
) map[libsveltosv1beta1.PolicyRef]bool {

	for i := range helmCharts {
		for j := range helmCharts[i].ValuesFrom {
			policyRefs[libsveltosv1beta1.PolicyRef{
				Namespace: helmCharts[i].ValuesFrom[j].Namespace,
				Name:      helmCharts[i].ValuesFrom[j].Name,
				Kind:      helmCharts[i].ValuesFrom[j].Kind,
			}] = true
		}
	}

	return policyRefs
}

func appendKustomizationRefValuesFrom(policyRefs map[libsveltosv1beta1.PolicyRef]bool,
	kustomizationRefs []configv1beta1.KustomizationRef) map[libsveltosv1beta1.PolicyRef]bool {

	for i := range kustomizationRefs {
		for j := range kustomizationRefs[i].ValuesFrom {
			policyRefs[libsveltosv1beta1.PolicyRef{
				Namespace: kustomizationRefs[i].ValuesFrom[j].Namespace,
				Name:      kustomizationRefs[i].ValuesFrom[j].Name,
				Kind:      kustomizationRefs[i].ValuesFrom[j].Kind,
			}] = true
		}
	}

	return policyRefs
}

// removeConfigMaps fetches all ConfigMaps created by EventTrigger instance for a given cluster.
// It deletes all stale ConfigMaps (all ConfigMap instances currently present and not in the policyRefs
// list).
// policyRefs arg represents all the ConfigMap the EventTrigger instance is currently managing for the
// given cluster
func removeConfigMaps(ctx context.Context, c client.Client, clusterNamespace, clusterName string,
	clusterType libsveltosv1beta1.ClusterType, eventTrigger *v1beta1.EventTrigger, er *libsveltosv1beta1.EventReport,
	policyRefs map[libsveltosv1beta1.PolicyRef]bool, logger logr.Logger) error {

	labels := getInstantiatedObjectLabels(clusterNamespace, clusterName, eventTrigger.Name,
		er, clusterType)

	listOptions := []client.ListOption{
		client.MatchingLabels(labels),
		client.InNamespace(ReportNamespace),
	}

	configMaps := &corev1.ConfigMapList{}
	err := c.List(ctx, configMaps, listOptions...)
	if err != nil {
		return err
	}

	for i := range configMaps.Items {
		cm := &configMaps.Items[i]
		if _, ok := policyRefs[*getPolicyRef(cm)]; !ok {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("deleting configMap %s", cm.Name))
			err = c.Delete(ctx, cm)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// removeSecrets fetches all Secrets created by EventTrigger instance for a given cluster.
// It deletes all stale Secrets (all Secret instances currently present and not in the policyRefs
// list).
// policyRefs arg represents all the ConfigMap the EventTrigger instance is currently managing for the
// given cluster
func removeSecrets(ctx context.Context, c client.Client, clusterNamespace, clusterName string,
	clusterType libsveltosv1beta1.ClusterType, eventTrigger *v1beta1.EventTrigger, er *libsveltosv1beta1.EventReport,
	policyRefs map[libsveltosv1beta1.PolicyRef]bool, logger logr.Logger) error {

	labels := getInstantiatedObjectLabels(clusterNamespace, clusterName, eventTrigger.Name,
		er, clusterType)

	listOptions := []client.ListOption{
		client.MatchingLabels(labels),
		client.InNamespace(ReportNamespace),
	}

	secrets := &corev1.SecretList{}
	err := c.List(ctx, secrets, listOptions...)
	if err != nil {
		return err
	}

	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if _, ok := policyRefs[*getPolicyRef(secret)]; !ok {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("deleting secret %s", secret.Name))
			err = c.Delete(ctx, secret)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// removeClusterProfiles fetches all ClusterProfiles created by EventTrigger instance for a given cluster.
// It deletes all stale ClusterProfiles (all ClusterProfile instances currently present and not in the clusterProfiles
// list).
// clusterProfiles arg represents all the ClusterProfiles the EventTrigger instance is currently managing for the
// given cluster
func removeClusterProfiles(ctx context.Context, c client.Client, clusterNamespace, clusterName string,
	clusterType libsveltosv1beta1.ClusterType, eventTrigger *v1beta1.EventTrigger, er *libsveltosv1beta1.EventReport,
	clusterProfiles []*configv1beta1.ClusterProfile, logger logr.Logger) error {

	// Build a map of current ClusterProfiles for faster indexing
	// Those are the clusterProfiles current eventTrigger instance is programming
	// for this cluster and need to not be removed
	currentClusterProfiles := make(map[string]bool)
	for i := range clusterProfiles {
		currentClusterProfiles[clusterProfiles[i].Name] = true
	}

	labels := getInstantiatedObjectLabels(clusterNamespace, clusterName, eventTrigger.Name,
		er, clusterType)

	listOptions := []client.ListOption{
		client.MatchingLabels(labels),
	}

	clusterProfileList := &configv1beta1.ClusterProfileList{}
	err := c.List(ctx, clusterProfileList, listOptions...)
	if err != nil {
		return err
	}

	for i := range clusterProfileList.Items {
		cp := &clusterProfileList.Items[i]
		if _, ok := currentClusterProfiles[cp.Name]; !ok {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("deleting clusterProfile %s", cp.Name))
			err = c.Delete(ctx, cp)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func unstructuredToTyped(config *rest.Config, u *unstructured.Unstructured) (runtime.Object, error) {
	obj, err := scheme.Scheme.New(u.GroupVersionKind())
	if err != nil {
		return nil, err
	}

	unstructuredContent := u.UnstructuredContent()
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredContent, &obj)
	if err != nil {
		return nil, err
	}

	return obj, nil
}

// fecthClusterObjects fetches resources representing a cluster.
// All fetched objects are in the management cluster.
// Currently limited to Cluster and Infrastructure Provider
func fecthClusterObjects(ctx context.Context, c client.Client,
	clusterNamespace, clusterName string, clusterType libsveltosv1beta1.ClusterType,
	logger logr.Logger) (map[string]interface{}, error) {

	logger.V(logs.LogInfo).Info(fmt.Sprintf("Fetch cluster %s: %s/%s",
		clusterType, clusterNamespace, clusterName))

	genericCluster, err := clusterproxy.GetCluster(ctx, c, clusterNamespace, clusterName, clusterType)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to fetch cluster %v", err))
		return nil, err
	}
	return runtime.DefaultUnstructuredConverter.ToUnstructured(genericCluster)
}

func getNonInstantiatedClusterProfile(eventTrigger *v1beta1.EventTrigger,
	clusterProfileName string, labels map[string]string) *configv1beta1.ClusterProfile {

	return &configv1beta1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:   clusterProfileName,
			Labels: labels,
		},
		Spec: configv1beta1.Spec{
			StopMatchingBehavior: eventTrigger.Spec.StopMatchingBehavior,
			SyncMode:             eventTrigger.Spec.SyncMode,
			Tier:                 eventTrigger.Spec.Tier,
			ContinueOnConflict:   eventTrigger.Spec.ContinueOnConflict,
			Reloader:             eventTrigger.Spec.Reloader,
			MaxUpdate:            eventTrigger.Spec.MaxUpdate,
			TemplateResourceRefs: nil, // this needs to be instantiated
			ValidateHealths:      eventTrigger.Spec.ValidateHealths,
			Patches:              eventTrigger.Spec.Patches,
			ExtraLabels:          eventTrigger.Spec.ExtraLabels,
			ExtraAnnotations:     eventTrigger.Spec.ExtraAnnotations,
		},
	}
}

func prepareCurrentObjects(ctx context.Context, c client.Client, clusterNamespace, clusterName string,
	clusterType libsveltosv1beta1.ClusterType, eventReport *libsveltosv1beta1.EventReport,
	resources []unstructured.Unstructured, logger logr.Logger) (*currentObjects, error) {

	values := make([]map[string]interface{}, len(resources))
	for i := range resources {
		values[i] = resources[i].UnstructuredContent()
	}
	cluster, err := fecthClusterObjects(ctx, c, clusterNamespace, clusterName, clusterType, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get cluster %v", err))
		return nil, err
	}

	return &currentObjects{
		MatchingResources: eventReport.Spec.MatchingResources,
		Resources:         values,
		Cluster:           cluster,
	}, nil
}

func prepareCurrentObject(ctx context.Context, c client.Client, clusterNamespace, clusterName string,
	clusterType libsveltosv1beta1.ClusterType, resource *unstructured.Unstructured,
	matchingResource *corev1.ObjectReference, logger logr.Logger) (*currentObject, error) {

	object := &currentObject{
		MatchingResource: *matchingResource,
	}
	if resource != nil {
		object.Resource = resource.UnstructuredContent()
	}
	cluster, err := fecthClusterObjects(ctx, c, clusterNamespace, clusterName, clusterType, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get cluster %v", err))
		return nil, err
	}
	object.Cluster = cluster

	return object, nil
}
