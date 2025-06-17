// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !plan9

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"tailscale.com/internal/client/tailscale"
	tsoperator "tailscale.com/k8s-operator"
	tsapi "tailscale.com/k8s-operator/apis/v1alpha1"
	"tailscale.com/kube/ingressservices"
	"tailscale.com/kube/k8s-proxy/conf"
	"tailscale.com/tailcfg"
	"tailscale.com/tstime"
	"tailscale.com/types/opt"
	"tailscale.com/util/clientmetric"
	"tailscale.com/util/mak"
)

const (
	proxyPGFinalizerName = "tailscale.com/proxy-pg-finalizer"

	reasonAPIServerProxyPGInvalid              = "APIServerProxyPGInvalid"
	reasonAPIServerProxyPGValid                = "APIServerProxyPGValid"
	reasonAPIServerProxyPGConfigured           = "APIServerProxyPGConfigured"
	reasonAPIServerProxyPGNoBackendsConfigured = "APIServerProxyPGNoBackendsConfigured"
	reasonAPIServerProxyPGCreationFailed       = "APIServerProxyPGCreationFailed"
)

var gaugeAPIServerProxyPGResources = clientmetric.NewGauge("tailscale_k8s_op_apiserver_pg_count")

// APIServerProxyServiceReconciler reconciles the Tailscale Services required for an
// HA deployment of the API Server Proxy.
type APIServerProxyServiceReconciler struct {
	client.Client
	recorder    record.EventRecorder
	logger      *zap.SugaredLogger
	tsClient    tsClient
	tsNamespace string
	defaultTags []string
	operatorID  string // stableID of the operator's Tailscale device

	clock tstime.Clock
}

// Reconcile is the entry point for the controller.
func (r *APIServerProxyServiceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (res reconcile.Result, err error) {
	logger := r.logger.With("ProxyGroup", req.Name)
	logger.Debugf("starting reconcile")
	defer logger.Debugf("reconcile finished")

	pg := new(tsapi.ProxyGroup)
	err = r.Get(ctx, req.NamespacedName, pg)
	if apierrors.IsNotFound(err) {
		// Request object not found, could have been deleted after reconcile request.
		logger.Debugf("ProxyGroup not found, assuming it was deleted")
		return res, nil
	} else if err != nil {
		return res, fmt.Errorf("failed to get ProxyGroup: %w", err)
	}

	serviceName := serviceNameForProxyGroup(pg)
	logger = logger.With("Tailscale Service", serviceName)

	if markedForDeletion(pg) {
		logger.Debugf("ProxyGroup is being deleted, ensuring any created resources are cleaned up")
		_, err = r.maybeCleanup(ctx, serviceName, pg, logger)
		return res, err
	}

	// needsRequeue is set to true if the underlying Tailscale Service has changed as a result of this reconcile. If that
	// is the case, we reconcile the Ingress one more time to ensure that concurrent updates to the Tailscale Service in a
	// multi-cluster Ingress setup have not resulted in another actor overwriting our Tailscale Service update.
	needsRequeue := false
	needsRequeue, err = r.maybeProvision(ctx, serviceName, pg, logger)
	if err != nil {
		if strings.Contains(err.Error(), optimisticLockErrorMsg) {
			logger.Infof("optimistic lock error, retrying: %s", err)
		} else {
			return reconcile.Result{}, err
		}
	}
	if needsRequeue {
		res = reconcile.Result{RequeueAfter: requeueInterval()}
	}

	return reconcile.Result{}, nil
}

// maybeProvision ensures that a Tailscale Service for this ProxyGroup exists
// and is up to date.
//
// Returns true if the operation resulted in a Tailscale Service update.
func (r *APIServerProxyServiceReconciler) maybeProvision(ctx context.Context, hostname string, pg *tsapi.ProxyGroup, logger *zap.SugaredLogger) (svcsChanged bool, err error) {
	oldPGStatus := pg.Status.DeepCopy()
	defer func() {
		if !apiequality.Semantic.DeepEqual(oldPGStatus, &pg.Status) {
			// An error encountered here should get returned by the Reconcile function.
			err = errors.Join(err, r.Client.Status().Update(ctx, pg))
		}
	}()

	if !tsoperator.ProxyGroupAvailable(pg) {
		logger.Infof("ProxyGroup is not (yet) available")
		return false, nil
	}

	if !slices.Contains(pg.Finalizers, proxyPGFinalizerName) {
		// This log line is printed exactly once during initial provisioning,
		// because once the finalizer is in place this block gets skipped. So,
		// this is a nice place to tell the operator that the high level,
		// multi-reconcile operation is underway.
		logger.Info("provisioning Tailscale Service for ProxyGroup")
		pg.Finalizers = append(pg.Finalizers, proxyPGFinalizerName)
		if err := r.Update(ctx, pg); err != nil {
			return false, fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	// 1. Ensure that there isn't a Tailscale Service with the same hostname
	// already created and not owned by this ProxyGroup.
	serviceName := tailcfg.ServiceName("svc:" + hostname)
	existingTSSvc, err := r.tsClient.GetVIPService(ctx, serviceName)
	if isErrorFeatureFlagNotEnabled(err) {
		logger.Warn(msgFeatureFlagNotEnabled)
		r.recorder.Event(pg, corev1.EventTypeWarning, warningTailscaleServiceFeatureFlagNotEnabled, msgFeatureFlagNotEnabled)
		return false, nil
	}
	if err != nil && !isErrorTailscaleServiceNotFound(err) {
		return false, fmt.Errorf("error getting Tailscale Service %q: %w", hostname, err)
	}

	// 2. Generate the Tailscale Service owner annotation for new or existing Tailscale Service.
	// This checks and ensures that Tailscale Service's owner references are updated
	// for this Service and errors if that is not possible (i.e. because it
	// appears that the Tailscale Service has been created by a non-operator actor or
	// is already owned by another operator).
	updatedAnnotations, err := exclusiveOwnerAnnotations(r.operatorID, existingTSSvc)
	if err != nil {
		const instr = "To proceed, you can either manually delete the existing Tailscale Service or choose a different Service name in the ProxyGroup's spec.kubeAPIServer.serviceName field"
		msg := fmt.Sprintf("error ensuring ownership of Tailscale Service %s: %v. %s", hostname, err, instr)
		logger.Warn(msg)
		r.recorder.Event(pg, corev1.EventTypeWarning, "InvalidTailscaleService", msg)
		tsoperator.SetProxyGroupCondition(pg, tsapi.APIServerProxyReady, metav1.ConditionFalse, reasonAPIServerProxyPGInvalid, msg, pg.Generation, r.clock, logger)
		return false, nil
	}

	// Get service tags - use KubeAPIServerConfig.ServiceTags if set, or fall back to pg.Spec.Tags
	serviceTags := r.defaultTags
	if pg.Spec.KubeAPIServer != nil && len(pg.Spec.KubeAPIServer.ServiceTags) > 0 {
		serviceTags = pg.Spec.KubeAPIServer.ServiceTags.Stringify()
	}

	tsSvc := &tailscale.VIPService{
		Name:        serviceName,
		Tags:        serviceTags,
		Ports:       []string{"tcp:443"},
		Comment:     managedTSServiceComment,
		Annotations: updatedAnnotations,
	}
	if existingTSSvc != nil {
		tsSvc.Addrs = existingTSSvc.Addrs
	}
	// TODO(tomhjp): right now if two kube-apiserver ProxyGroup resources attempt
	// to apply different Tailscale Service configs (different tags) we can end
	// up reconciling those in a loop. We should detect when a Service with the
	// same generation number has been reconciled ~more than N times and stop
	// attempting to apply updates.
	if existingTSSvc == nil ||
		!slices.Equal(tsSvc.Tags, existingTSSvc.Tags) ||
		!ownersAreSetAndEqual(tsSvc, existingTSSvc) ||
		!slices.Equal(tsSvc.Ports, existingTSSvc.Ports) {
		logger.Infof("Ensuring Tailscale Service exists and is up to date")
		if err := r.tsClient.CreateOrUpdateVIPService(ctx, tsSvc); err != nil {
			return false, fmt.Errorf("error creating Tailscale Service: %w", err)
		}
		existingTSSvc = tsSvc
	}

	// Update the configs for the ProxyGroup pods to advertise the service
	if err = r.maybeUpdateAdvertiseServicesConfig(ctx, pg, serviceName, logger); err != nil {
		return false, fmt.Errorf("failed to update pod configs: %w", err)
	}

	// Check how many pods are advertising the service
	count, err := r.numberPodsAdvertising(ctx, pg.Name, serviceName)
	if err != nil {
		return false, fmt.Errorf("failed to get number of advertised Pods: %w", err)
	}

	// Update the ProxyGroup status with the Tailscale Service information
	// Update the condition based on how many pods are advertising the service
	conditionStatus := metav1.ConditionFalse
	conditionReason := reasonAPIServerProxyPGNoBackendsConfigured
	conditionMessage := fmt.Sprintf("%d/%d proxy backends ready and advertising", count, pgReplicas(pg))

	if count > 0 {
		// At least one pod is advertising the service, consider it configured
		conditionStatus = metav1.ConditionTrue
		conditionReason = reasonAPIServerProxyPGConfigured
	}

	tsoperator.SetProxyGroupCondition(pg, tsapi.APIServerProxyReady, conditionStatus, conditionReason, conditionMessage, pg.Generation, r.clock, logger)

	return svcsChanged, nil
}

// maybeCleanup ensures that any resources, such as a Tailscale Service created for this Service, are cleaned up when the
// Service is being deleted or is unexposed. The cleanup is safe for a multi-cluster setup- the Tailscale Service is only
// deleted if it does not contain any other owner references. If it does the cleanup only removes the owner reference
// corresponding to this Service.
func (r *APIServerProxyServiceReconciler) maybeCleanup(ctx context.Context, hostname string, pg *tsapi.ProxyGroup, logger *zap.SugaredLogger) (svcChanged bool, err error) {
	logger.Debugf("Ensuring any resources for ProxyGroup are cleaned up")
	ix := slices.Index(pg.Finalizers, finalizerName)
	if ix < 0 {
		logger.Debugf("no finalizer, nothing to do")
		return false, nil
	}
	logger.Infof("Ensuring that Tailscale ProxyGroup %q configuration is cleaned up", hostname)

	defer func() {
		if err != nil {
			return
		}
		err = r.deleteFinalizer(ctx, pg, logger)
	}()

	serviceName := tailcfg.ServiceName("svc:" + hostname)
	//  1. Clean up the Tailscale Service.
	svcChanged, err = cleanupTailscaleService(ctx, r.tsClient, serviceName, r.operatorID, logger)
	if err != nil {
		return false, fmt.Errorf("error deleting Tailscale Service: %w", err)
	}

	// 2. Unadvertise the Tailscale Service.
	pgName := pg.Annotations[AnnotationProxyGroup]
	if err = r.maybeUpdateAdvertiseServicesConfig(ctx, pg, serviceName, logger); err != nil {
		return false, fmt.Errorf("failed to update tailscaled config services: %w", err)
	}

	// TODO: maybe wait for the service to be unadvertised, only then remove the backend routing

	// 3. Clean up ingress config (routing rules).
	cm, cfgs, err := ingressSvcsConfigs(ctx, r.Client, pgName, r.tsNamespace)
	if err != nil {
		return false, fmt.Errorf("error retrieving ingress services configuration: %w", err)
	}
	if cm == nil || cfgs == nil {
		return true, nil
	}
	logger.Infof("Removing Tailscale Service %q from ingress config for ProxyGroup %q", hostname, pgName)
	delete(cfgs, serviceName.String())
	cfgBytes, err := json.Marshal(cfgs)
	if err != nil {
		return false, fmt.Errorf("error marshaling ingress config: %w", err)
	}
	mak.Set(&cm.BinaryData, ingressservices.IngressConfigKey, cfgBytes)
	return true, r.Update(ctx, cm)
}

// Tailscale Services that are associated with the provided ProxyGroup and no longer managed this operator's instance are deleted, if not owned by other operator instances, else the owner reference is cleaned up.
// Returns true if the operation resulted in existing Tailscale Service updates (owner reference removal).
func (r *APIServerProxyServiceReconciler) maybeCleanupProxyGroup(ctx context.Context, proxyGroupName string, logger *zap.SugaredLogger) (svcsChanged bool, err error) {
	// Get all services deployed by this ProxyGroup
	serviceName := tailcfg.ServiceName("svc:" + proxyGroupName)

	// Try to clean up the Tailscale Service
	existingTSSvc, err := r.tsClient.GetVIPService(ctx, serviceName)
	if err != nil && !isErrorTailscaleServiceNotFound(err) && !isErrorFeatureFlagNotEnabled(err) {
		return false, fmt.Errorf("error getting Tailscale Service %q: %w", serviceName, err)
	}

	// Delete the Tailscale Service if it exists
	if existingTSSvc != nil {
		logger.Infof("Deleting Tailscale Service %q", serviceName)

		// Clean up any pod configurations
		if err := r.tsClient.DeleteVIPService(ctx, serviceName); err != nil && !isErrorTailscaleServiceNotFound(err) {
			return false, fmt.Errorf("deleting Tailscale Service %q: %w", serviceName, err)
		}

		svcsChanged = true
	}

	return svcsChanged, nil
}

func (r *APIServerProxyServiceReconciler) deleteFinalizer(ctx context.Context, pg *tsapi.ProxyGroup, logger *zap.SugaredLogger) error {
	pg.Finalizers = slices.DeleteFunc(pg.Finalizers, func(f string) bool {
		return f == proxyPGFinalizerName
	})
	logger.Debugf("ensure %q finalizer is removed", proxyPGFinalizerName)

	if err := r.Update(ctx, pg); err != nil {
		return fmt.Errorf("failed to remove finalizer %q: %w", proxyPGFinalizerName, err)
	}
	return nil
}

func (r *APIServerProxyServiceReconciler) maybeUpdateAdvertiseServicesConfig(ctx context.Context, pg *tsapi.ProxyGroup, serviceName tailcfg.ServiceName, logger *zap.SugaredLogger) error {
	logger.Debugf("Updating advertisement for service '%s'", serviceName)
	defer logger.Debugf("Finished updating advertisement for service '%s'", serviceName)

	// Get all config Secrets for this ProxyGroup
	secrets := &corev1.SecretList{}
	if err := r.List(ctx, secrets, client.InNamespace(r.tsNamespace), client.MatchingLabels(pgSecretLabels(pg.Name, "config"))); err != nil {
		return fmt.Errorf("failed to list config Secrets: %w", err)
	}
	logger.Debugf("Found %d config Secrets for ProxyGroup %s", len(secrets.Items), pg.Name)

	// Get auth mode from KubeAPIServerConfig if set
	authMode := true // default
	if pg.Spec.KubeAPIServer != nil && pg.Spec.KubeAPIServer.Mode != nil {
		authMode = *pg.Spec.KubeAPIServer.Mode == tsapi.APIServerProxyModeAuth
	}

	// For each pod's config Secret, update its configuration
	for _, secret := range secrets.Items {
		logger.Infof("Processing config Secret %s", secret.Name)
		if len(secret.Data[kubeAPIServerConfigFile]) == 0 {
			continue
		}

		// Parse the existing config
		var cfg conf.VersionedConfig
		if err := json.Unmarshal(secret.Data[kubeAPIServerConfigFile], &cfg); err != nil {
			logger.Warnf("Failed to unmarshal config for %s: %v", secret.Name, err)
			continue
		}

		// Get pod name and index from the Secret name
		podName := strings.TrimPrefix(secret.Name, pg.Name+"-config-")
		podIndex := -1
		fmt.Sscanf(podName, "%d", &podIndex)

		// Determine if this is the first replica that should handle certificate provisioning
		doProvisioning := podIndex == 0

		// Create or update the KubeAPIServer configuration
		if cfg.ConfigV1Alpha1 == nil {
			cfg.ConfigV1Alpha1 = &conf.ConfigV1Alpha1{}
		}

		if cfg.ConfigV1Alpha1.KubeAPIServer == nil {
			cfg.ConfigV1Alpha1.KubeAPIServer = &conf.KubeAPIServer{
				AuthMode: opt.NewBool(authMode),
			}
		} else {
			cfg.ConfigV1Alpha1.KubeAPIServer.AuthMode = opt.NewBool(authMode)
		}

		// Configure the Tailscale Service
		cfg.ConfigV1Alpha1.KubeAPIServer.TailscaleService = &conf.TailscaleService{
			Name:           serviceName.WithoutPrefix(),
			DoProvisioning: doProvisioning,
		}

		// Update the config Secret
		updatedCfg, err := json.Marshal(cfg)
		if err != nil {
			logger.Warnf("Failed to marshal config for %s: %v", secret.Name, err)
			continue
		}

		secret.Data[kubeAPIServerConfigFile] = updatedCfg
		if err := r.Update(ctx, &secret); err != nil {
			logger.Warnf("Failed to update config Secret %s: %v", secret.Name, err)
		}
	}

	return nil
}

func (r *APIServerProxyServiceReconciler) numberPodsAdvertising(ctx context.Context, pgName string, serviceName tailcfg.ServiceName) (int, error) {
	// Get all pods for this ProxyGroup
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(r.tsNamespace), client.MatchingLabels{
		"app": pgName,
	}); err != nil {
		return 0, fmt.Errorf("failed to list pods for ProxyGroup %q: %w", pgName, err)
	}

	// Check how many pods are ready
	var count int
	for _, pod := range podList.Items {
		if isPodReady(&pod) {
			count++
		}
	}

	return count, nil
}

// isPodReady returns true if the pod is in ready state
func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func serviceNameForProxyGroup(pg *tsapi.ProxyGroup) string {
	if pg.Spec.KubeAPIServer != nil && pg.Spec.KubeAPIServer.ServiceName != "" {
		return pg.Spec.KubeAPIServer.ServiceName
	}
	return pg.Name
}

// exclusiveOwnerAnnotations returns the updated annotations required to ensure this
// instance of the operator is the exclusive owner. If the Tailscale Service is not
// nil, but does not contain an owner reference we return an error as this likely means
// that the Service was created by somthing other than a Tailscale Kubernetes operator.
// We also error if it is already owned by another operator instance, as we do not
// want to load balance a kube-apiserver ProxyGroup across multiple clusters.
func exclusiveOwnerAnnotations(operatorID string, svc *tailscale.VIPService) (map[string]string, error) {
	ref := OwnerRef{
		OperatorID: operatorID,
	}
	if svc == nil {
		c := ownerAnnotationValue{OwnerRefs: []OwnerRef{ref}}
		json, err := json.Marshal(c)
		if err != nil {
			return nil, fmt.Errorf("[unexpected] unable to marshal Tailscale Service's owner annotation contents: %w, please report this", err)
		}
		return map[string]string{
			ownerAnnotation: string(json),
		}, nil
	}
	o, err := parseOwnerAnnotation(svc)
	if err != nil {
		return nil, err
	}
	if o == nil || len(o.OwnerRefs) == 0 {
		return nil, fmt.Errorf("Tailscale Service %s exists, but does not contain owner annotation with owner references; not proceeding as this is likely a resource created by something other than the Tailscale Kubernetes operator", svc.Name)
	}
	if len(o.OwnerRefs) > 1 || o.OwnerRefs[0].OperatorID != operatorID {
		return nil, fmt.Errorf("Tailscale Service %s is already owned by other operator(s) and cannot be shared across multiple clusters; configure a difference Service name to continue", svc.Name)
	}

	return svc.Annotations, nil
}
