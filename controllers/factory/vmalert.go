package factory

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	victoriametricsv1beta1 "github.com/VictoriaMetrics/operator/api/v1beta1"
	"github.com/VictoriaMetrics/operator/controllers/factory/finalize"
	"github.com/VictoriaMetrics/operator/controllers/factory/k8stools"
	"github.com/VictoriaMetrics/operator/controllers/factory/psp"
	"github.com/VictoriaMetrics/operator/internal/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	vmAlertConfigDir        = "/etc/vmalert/config"
	datasourceKey           = "datasource"
	remoteReadKey           = "remoteRead"
	remoteWriteKey          = "remoteWrite"
	notifierConfigMountPath = `/etc/vm/notifier_config`
	vmalertConfigSecretsDir = "/etc/vmalert/remote_secrets"
	bearerTokenKey          = "bearerToken"
	basicAuthPasswordKey    = "basicAuthPassword"
	oauth2SecretKey         = "oauth2SecretKey"
)

func buildNotifierKey(idx int) string {
	return fmt.Sprintf("notifier-%d", idx)
}

func buildRemoteSecretKey(source, suffix string) string {
	return fmt.Sprintf("%s_%s", strings.ToUpper(source), strings.ToUpper(suffix))
}

func CreateOrUpdateVMAlertService(ctx context.Context, cr *victoriametricsv1beta1.VMAlert, rclient client.Client, c *config.BaseOperatorConf) (*corev1.Service, error) {
	if cr.Spec.Port == "" {
		cr.Spec.Port = c.VMAlertDefault.Port
	}
	additionalSvc := buildDefaultService(cr, cr.Spec.Port, nil)
	mergeServiceSpec(additionalSvc, cr.Spec.ServiceSpec)
	newService := buildDefaultService(cr, cr.Spec.Port, nil)

	// user may want to abuse it, if serviceSpec.name == crd.prefixedName,
	// log error?
	if cr.Spec.ServiceSpec != nil {
		if additionalSvc.Name == newService.Name {
			log.Error(fmt.Errorf("vmalert additional service name: %q cannot be the same as crd.prefixedname: %q", additionalSvc.Name, cr.PrefixedName()), "cannot create additional service")
		} else if _, err := reconcileServiceForCRD(ctx, rclient, additionalSvc); err != nil {
			return nil, err
		}
	}
	rca := finalize.RemoveSvcArgs{
		PrefixedName:   cr.PrefixedName,
		GetNameSpace:   cr.GetNamespace,
		SelectorLabels: cr.SelectorLabels,
	}
	if err := finalize.RemoveOrphanedServices(ctx, rclient, rca, cr.Spec.ServiceSpec); err != nil {
		return nil, err
	}

	return reconcileServiceForCRD(ctx, rclient, newService)
}

func createOrUpdateVMAlertSecret(ctx context.Context, rclient client.Client, cr *victoriametricsv1beta1.VMAlert, ssCache map[string]*authSecret, c *config.BaseOperatorConf) error {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cr.PrefixedName(),
			Annotations:     cr.AnnotationsFiltered(),
			Labels:          c.Labels.Merge(cr.AllLabels()),
			Namespace:       cr.Namespace,
			OwnerReferences: cr.AsOwner(),
		},
		Data: map[string][]byte{},
	}
	addSecretKeys := func(sourcePrefix string, ha victoriametricsv1beta1.HTTPAuth) {
		ba := ssCache[sourcePrefix]
		if ba == nil {
			return
		}
		if ha.BasicAuth != nil && ba.BasicAuthCredentials != nil {
			if len(ba.password) > 0 {
				s.Data[buildRemoteSecretKey(sourcePrefix, basicAuthPasswordKey)] = []byte(ba.password)
			}
		}
		if ha.BearerAuth != nil && len(ba.bearerValue) > 0 {
			s.Data[buildRemoteSecretKey(sourcePrefix, bearerTokenKey)] = []byte(ba.bearerValue)
		}
		if ha.OAuth2 != nil && ba.oauthCreds != nil {
			if len(ba.clientSecret) > 0 {
				s.Data[buildRemoteSecretKey(sourcePrefix, oauth2SecretKey)] = []byte(ba.clientSecret)
			}
		}
	}
	if cr.Spec.RemoteRead != nil {
		addSecretKeys(remoteReadKey, cr.Spec.RemoteRead.HTTPAuth)
	}
	if cr.Spec.RemoteWrite != nil {
		addSecretKeys(remoteWriteKey, cr.Spec.RemoteWrite.HTTPAuth)
	}
	addSecretKeys(datasourceKey, cr.Spec.Datasource.HTTPAuth)
	for idx, nf := range cr.Spec.Notifiers {
		addSecretKeys(buildNotifierKey(idx), nf.HTTPAuth)
	}

	curSecret := &corev1.Secret{}
	err := rclient.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: s.Name}, curSecret)
	if errors.IsNotFound(err) {
		if err = rclient.Create(ctx, s); err != nil {
			return fmt.Errorf("cannot create secret for vmalert remote secrets: %w", err)
		}
		return nil
	}
	s.Annotations = labels.Merge(curSecret.Annotations, s.Annotations)
	return rclient.Update(ctx, s)
}

func CreateOrUpdateVMAlert(ctx context.Context, cr *victoriametricsv1beta1.VMAlert, rclient client.Client, c *config.BaseOperatorConf, cmNames []string) error {
	l := log.WithValues("controller", "vmalert.crud", "vmalert", cr.Name)
	// copy to avoid side effects.
	cr = cr.DeepCopy()
	var additionalNotifiers []victoriametricsv1beta1.VMAlertNotifierSpec

	if cr.Spec.Notifier != nil {
		cr.Spec.Notifiers = append(cr.Spec.Notifiers, *cr.Spec.Notifier)
	}
	// trim notifiers with non-empty notifier Selector
	var cnt int
	for i := range cr.Spec.Notifiers {
		n := cr.Spec.Notifiers[i]
		// fast path
		if n.Selector == nil {
			cr.Spec.Notifiers[cnt] = n
			cnt++
			continue
		}
		// discover alertmanagers
		var ams victoriametricsv1beta1.VMAlertmanagerList
		amListOpts, err := n.Selector.AsListOptions()
		if err != nil {
			return fmt.Errorf("cannot convert notifier selector as ListOptions: %w", err)
		}
		if err := rclient.List(ctx, &ams, amListOpts, config.MustGetNamespaceListOptions()); err != nil {
			return fmt.Errorf("cannot list alertmanagers for vmalert notifier sd: %w", err)
		}
		for _, item := range ams.Items {
			if !item.DeletionTimestamp.IsZero() || (n.Selector.Namespace != nil && !n.Selector.Namespace.IsMatch(&item)) {
				continue
			}
			dsc := item.AsNotifiers()
			additionalNotifiers = append(additionalNotifiers, dsc...)
		}
	}
	cr.Spec.Notifiers = cr.Spec.Notifiers[:cnt]

	if len(additionalNotifiers) > 0 {
		sort.Slice(additionalNotifiers, func(i, j int) bool {
			return additionalNotifiers[i].URL > additionalNotifiers[j].URL
		})
		l.Info("additional notifiers with sd selector", "len", len(additionalNotifiers))
	}
	cr.Spec.Notifiers = append(cr.Spec.Notifiers, additionalNotifiers...)

	if err := psp.CreateServiceAccountForCRD(ctx, cr, rclient); err != nil {
		return fmt.Errorf("failed create service account: %w", err)
	}
	if c.PSPAutoCreateEnabled {
		if err := psp.CreateOrUpdateServiceAccountWithPSP(ctx, cr, rclient); err != nil {
			return fmt.Errorf("cannot create podsecurity policy for vmalert, err=%w", err)
		}
	}
	remoteSecrets, err := loadVMAlertRemoteSecrets(ctx, rclient, cr)
	if err != nil {
		return err
	}
	// create secret for remoteSecrets
	if err := createOrUpdateVMAlertSecret(ctx, rclient, cr, remoteSecrets, c); err != nil {
		return err
	}

	if cr.Spec.PodDisruptionBudget != nil {
		if err := CreateOrUpdatePodDisruptionBudget(ctx, rclient, cr, cr.Kind, cr.Spec.PodDisruptionBudget); err != nil {
			return fmt.Errorf("cannot update pod disruption budget for vmalert: %w", err)
		}
	}

	err = CreateOrUpdateTlsAssetsForVMAlert(ctx, cr, rclient)
	if err != nil {
		return err
	}
	newDeploy, err := newDeployForVMAlert(cr, c, cmNames, remoteSecrets)
	if err != nil {
		return fmt.Errorf("cannot generate new deploy for vmalert: %w", err)
	}

	currDeploy := &appsv1.Deployment{}
	err = rclient.Get(ctx, types.NamespacedName{Namespace: newDeploy.Namespace, Name: newDeploy.Name}, currDeploy)
	if err != nil {
		if errors.IsNotFound(err) {
			err := rclient.Create(ctx, newDeploy)
			if err != nil {
				return fmt.Errorf("cannot create vmalert deploy: %w", err)
			}
		} else {
			return fmt.Errorf("cannot get deploy for vmalert: %w", err)
		}
	}
	newDeploy.Annotations = labels.Merge(currDeploy.Annotations, newDeploy.Annotations)
	newDeploy.Finalizers = victoriametricsv1beta1.MergeFinalizers(currDeploy, victoriametricsv1beta1.FinalizerName)
	if err := rclient.Update(ctx, newDeploy); err != nil {
		return fmt.Errorf("cannot update vmalert deploy: %w", err)
	}
	return nil
}

// newDeployForCR returns a busybox pod with the same name/namespace as the cr
func newDeployForVMAlert(cr *victoriametricsv1beta1.VMAlert, c *config.BaseOperatorConf, ruleConfigMapNames []string, remoteSecrets map[string]*authSecret) (*appsv1.Deployment, error) {
	if cr.Spec.Image.Repository == "" {
		cr.Spec.Image.Repository = c.VMAlertDefault.Image
	}
	if cr.Spec.Image.Tag == "" {
		cr.Spec.Image.Tag = c.VMAlertDefault.Version
	}
	if cr.Spec.Image.PullPolicy == "" {
		cr.Spec.Image.PullPolicy = corev1.PullIfNotPresent
	}

	if cr.Spec.Port == "" {
		cr.Spec.Port = c.VMAlertDefault.Port
	}

	generatedSpec, err := vmAlertSpecGen(cr, c, ruleConfigMapNames, remoteSecrets)
	if err != nil {
		return nil, fmt.Errorf("cannot generate new spec for vmalert: %w", err)
	}

	if cr.Spec.ImagePullSecrets != nil && len(cr.Spec.ImagePullSecrets) > 0 {
		generatedSpec.Template.Spec.ImagePullSecrets = cr.Spec.ImagePullSecrets
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cr.PrefixedName(),
			Namespace:       cr.Namespace,
			Labels:          c.Labels.Merge(cr.AllLabels()),
			Annotations:     cr.AnnotationsFiltered(),
			OwnerReferences: cr.AsOwner(),
			Finalizers:      []string{victoriametricsv1beta1.FinalizerName},
		},
		Spec: *generatedSpec,
	}
	return deploy, nil
}

func vmAlertSpecGen(cr *victoriametricsv1beta1.VMAlert, c *config.BaseOperatorConf, ruleConfigMapNames []string, remoteSecrets map[string]*authSecret) (*appsv1.DeploymentSpec, error) {
	confReloadArgs := []string{
		fmt.Sprintf("-webhook-url=%s", victoriametricsv1beta1.BuildReloadPathWithPort(cr.Spec.ExtraArgs, cr.Spec.Port)),
	}
	for _, cm := range ruleConfigMapNames {
		confReloadArgs = append(confReloadArgs, fmt.Sprintf("-volume-dir=%s", path.Join(vmAlertConfigDir, cm)))
	}

	args := buildVMAlertArgs(cr, ruleConfigMapNames, remoteSecrets)

	var envs []corev1.EnvVar

	envs = append(envs, cr.Spec.ExtraEnvs...)

	var volumes []corev1.Volume
	volumes = append(volumes, cr.Spec.Volumes...)

	volumes = append(volumes, corev1.Volume{
		Name: "tls-assets",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: cr.TLSAssetName(),
			},
		},
	},
		corev1.Volume{
			Name: "remote-secrets",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: cr.PrefixedName(),
				},
			},
		},
	)

	for _, name := range ruleConfigMapNames {
		volumes = append(volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: name,
					},
				},
			},
		})
	}

	var volumeMounts []corev1.VolumeMount
	volumeMounts = append(volumeMounts, cr.Spec.VolumeMounts...)
	volumeMounts = append(volumeMounts, corev1.VolumeMount{
		Name:      "tls-assets",
		ReadOnly:  true,
		MountPath: tlsAssetsDir,
	},
		corev1.VolumeMount{
			Name:      "remote-secrets",
			ReadOnly:  true,
			MountPath: vmalertConfigSecretsDir,
		},
	)

	volumes, volumeMounts = cr.Spec.License.MaybeAddToVolumes(volumes, volumeMounts, SecretsDir)

	if cr.Spec.NotifierConfigRef != nil {
		volumes = append(volumes, corev1.Volume{
			Name: "vmalert-notifier-config",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: cr.Spec.NotifierConfigRef.Name,
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "vmalert-notifier-config",
			MountPath: notifierConfigMountPath,
		})
	}
	for _, s := range cr.Spec.Secrets {
		volumes = append(volumes, corev1.Volume{
			Name: k8stools.SanitizeVolumeName("secret-" + s),
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: s,
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      k8stools.SanitizeVolumeName("secret-" + s),
			ReadOnly:  true,
			MountPath: path.Join(SecretsDir, s),
		})
	}

	for _, c := range cr.Spec.ConfigMaps {
		volumes = append(volumes, corev1.Volume{
			Name: k8stools.SanitizeVolumeName("configmap-" + c),
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: c,
					},
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      k8stools.SanitizeVolumeName("configmap-" + c),
			ReadOnly:  true,
			MountPath: path.Join(ConfigMapsDir, c),
		})
	}

	for _, name := range ruleConfigMapNames {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      name,
			MountPath: path.Join(vmAlertConfigDir, name),
		})
	}
	var reloaderVolumes []corev1.VolumeMount
	for _, name := range ruleConfigMapNames {
		reloaderVolumes = append(reloaderVolumes, corev1.VolumeMount{
			Name:      name,
			MountPath: path.Join(vmAlertConfigDir, name),
		})
	}

	resources := corev1.ResourceRequirements{Limits: corev1.ResourceList{}, Requests: corev1.ResourceList{}}
	if c.VMAlertDefault.ConfigReloaderCPU != "0" && c.VMAgentDefault.UseDefaultResources {
		resources.Limits[corev1.ResourceCPU] = resource.MustParse(c.VMAlertDefault.ConfigReloaderCPU)
		resources.Requests[corev1.ResourceCPU] = resource.MustParse(c.VMAlertDefault.ConfigReloaderCPU)
	}
	if c.VMAlertDefault.ConfigReloaderMemory != "0" && c.VMAgentDefault.UseDefaultResources {
		resources.Limits[corev1.ResourceMemory] = resource.MustParse(c.VMAlertDefault.ConfigReloaderMemory)
		resources.Requests[corev1.ResourceMemory] = resource.MustParse(c.VMAlertDefault.ConfigReloaderMemory)
	}

	var ports []corev1.ContainerPort
	ports = append(ports, corev1.ContainerPort{Name: "http", Protocol: "TCP", ContainerPort: intstr.Parse(cr.Spec.Port).IntVal})

	// sort for consistency
	sort.Strings(args)
	sort.Slice(volumes, func(i, j int) bool {
		return volumes[i].Name < volumes[j].Name
	})
	sort.Slice(volumeMounts, func(i, j int) bool {
		return volumeMounts[i].Name < volumeMounts[j].Name
	})
	sort.Slice(reloaderVolumes, func(i, j int) bool {
		return reloaderVolumes[i].Name < reloaderVolumes[j].Name
	})
	sort.Strings(confReloadArgs)

	vmalertContainer := corev1.Container{
		Args:                     args,
		Name:                     "vmalert",
		Image:                    fmt.Sprintf("%s:%s", formatContainerImage(c.ContainerRegistry, cr.Spec.Image.Repository), cr.Spec.Image.Tag),
		ImagePullPolicy:          cr.Spec.Image.PullPolicy,
		Ports:                    ports,
		VolumeMounts:             volumeMounts,
		Resources:                buildResources(cr.Spec.Resources, config.Resource(c.VMAlertDefault.Resource), c.VMAlertDefault.UseDefaultResources),
		Env:                      envs,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	vmalertContainer = buildProbe(vmalertContainer, cr)

	vmalertContainers := []corev1.Container{
		vmalertContainer, {
			Name:                     "config-reloader",
			Image:                    fmt.Sprintf("%s", formatContainerImage(c.ContainerRegistry, c.VMAlertDefault.ConfigReloadImage)),
			Args:                     confReloadArgs,
			Resources:                resources,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			VolumeMounts:             reloaderVolumes,
		},
	}

	containers, err := k8stools.MergePatchContainers(vmalertContainers, cr.Spec.Containers)
	if err != nil {
		return nil, err
	}

	strategyType := appsv1.RollingUpdateDeploymentStrategyType
	if cr.Spec.UpdateStrategy != nil {
		strategyType = *cr.Spec.UpdateStrategy
	}

	for i := range cr.Spec.TopologySpreadConstraints {
		if cr.Spec.TopologySpreadConstraints[i].LabelSelector == nil {
			cr.Spec.TopologySpreadConstraints[i].LabelSelector = &metav1.LabelSelector{
				MatchLabels: cr.SelectorLabels(),
			}
		}
	}

	useStrictSecurity := c.EnableStrictSecurity
	if cr.Spec.UseStrictSecurity != nil {
		useStrictSecurity = *cr.Spec.UseStrictSecurity
	}

	spec := &appsv1.DeploymentSpec{
		Replicas:             cr.Spec.ReplicaCount,
		MinReadySeconds:      cr.Spec.MinReadySeconds,
		RevisionHistoryLimit: cr.Spec.RevisionHistoryLimitCount,

		Selector: &metav1.LabelSelector{
			MatchLabels: cr.SelectorLabels(),
		},

		Strategy: appsv1.DeploymentStrategy{
			Type:          strategyType,
			RollingUpdate: cr.Spec.RollingUpdate,
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      cr.PodLabels(),
				Annotations: cr.PodAnnotations(),
			},
			Spec: corev1.PodSpec{
				NodeSelector:                  cr.Spec.NodeSelector,
				SchedulerName:                 cr.Spec.SchedulerName,
				RuntimeClassName:              cr.Spec.RuntimeClassName,
				ServiceAccountName:            cr.GetServiceAccountName(),
				InitContainers:                addStrictSecuritySettingsToContainers(cr.Spec.InitContainers, useStrictSecurity),
				Containers:                    addStrictSecuritySettingsToContainers(containers, useStrictSecurity),
				Volumes:                       volumes,
				PriorityClassName:             cr.Spec.PriorityClassName,
				SecurityContext:               addStrictSecuritySettingsToPod(cr.Spec.SecurityContext, useStrictSecurity),
				Affinity:                      cr.Spec.Affinity,
				Tolerations:                   cr.Spec.Tolerations,
				HostNetwork:                   cr.Spec.HostNetwork,
				DNSPolicy:                     cr.Spec.DNSPolicy,
				DNSConfig:                     cr.Spec.DNSConfig,
				TopologySpreadConstraints:     cr.Spec.TopologySpreadConstraints,
				TerminationGracePeriodSeconds: cr.Spec.TerminationGracePeriodSeconds,
				ReadinessGates:                cr.Spec.ReadinessGates,
			},
		},
	}
	return spec, nil
}

func buildHeadersArg(flagName string, src []string, headers []string) []string {
	if len(headers) == 0 {
		return src
	}
	var headerFlagValue string
	for _, headerKV := range headers {
		headerFlagValue += headerKV + "^^"
	}
	headerFlagValue = strings.TrimSuffix(headerFlagValue, "^^")
	src = append(src, fmt.Sprintf("--%s=%s", flagName, headerFlagValue))
	return src
}

func buildVMAlertAuthArgs(args []string, flagPrefix string, ha victoriametricsv1beta1.HTTPAuth, remoteSecrets map[string]*authSecret) []string {
	if s, ok := remoteSecrets[flagPrefix]; ok {
		// safety checks must be performed by previous code
		if ha.BasicAuth != nil {
			args = append(args, fmt.Sprintf("-%s.basicAuth.username=%s", flagPrefix, s.username))
			if len(s.password) > 0 {
				args = append(args, fmt.Sprintf("-%s.basicAuth.passwordFile=%s", flagPrefix, path.Join(vmalertConfigSecretsDir, buildRemoteSecretKey(flagPrefix, basicAuthPasswordKey))))
			}
			if len(ha.BasicAuth.PasswordFile) > 0 {
				args = append(args, fmt.Sprintf("-%s.basicAuth.passwordFile=%s", flagPrefix, ha.BasicAuth.PasswordFile))
			}
		}
		if ha.BearerAuth != nil {
			if len(s.bearerValue) > 0 {
				args = append(args, fmt.Sprintf("-%s.bearerTokenFile=%s", flagPrefix, path.Join(vmalertConfigSecretsDir, buildRemoteSecretKey(flagPrefix, bearerTokenKey))))
			} else if len(ha.BearerAuth.TokenFilePath) > 0 {
				args = append(args, fmt.Sprintf("-%s.bearerTokenFile=%s", flagPrefix, ha.BearerAuth.TokenFilePath))
			}
		}
		if ha.OAuth2 != nil {
			if len(ha.OAuth2.ClientSecretFile) > 0 {
				args = append(args, fmt.Sprintf("-%s.oauth2.clientSecretFile=%s", flagPrefix, ha.OAuth2.ClientSecretFile))
			} else {
				args = append(args, fmt.Sprintf("-%s.oauth2.clientSecretFile=%s", flagPrefix, path.Join(vmalertConfigSecretsDir, buildRemoteSecretKey(flagPrefix, oauth2SecretKey))))
			}
			args = append(args, fmt.Sprintf("-%s.oauth2.clientID=%s", flagPrefix, s.oauthCreds.clientID))
			args = append(args, fmt.Sprintf("-%s.oauth2.tokenUrl=%s", flagPrefix, ha.OAuth2.TokenURL))
			args = append(args, fmt.Sprintf("-%s.oauth2.scopes=%s", flagPrefix, strings.Join(ha.OAuth2.Scopes, ",")))
		}
	}

	return args
}

func buildVMAlertArgs(cr *victoriametricsv1beta1.VMAlert, ruleConfigMapNames []string, remoteSecrets map[string]*authSecret) []string {
	pathPrefix := path.Join(tlsAssetsDir, cr.Namespace)
	args := []string{
		fmt.Sprintf("-datasource.url=%s", cr.Spec.Datasource.URL),
	}

	args = buildHeadersArg("datasource.headers", args, cr.Spec.Datasource.HTTPAuth.Headers)
	args = append(args, BuildNotifiersArgs(cr, remoteSecrets)...)
	args = buildVMAlertAuthArgs(args, datasourceKey, cr.Spec.Datasource.HTTPAuth, remoteSecrets)

	if cr.Spec.Datasource.HTTPAuth.TLSConfig != nil {
		tlsConf := cr.Spec.Datasource.HTTPAuth.TLSConfig
		args = tlsConf.AsArgs(args, datasourceKey, pathPrefix)
	}

	if cr.Spec.RemoteWrite != nil {
		args = append(args, fmt.Sprintf("-remoteWrite.url=%s", cr.Spec.RemoteWrite.URL))
		args = buildVMAlertAuthArgs(args, remoteWriteKey, cr.Spec.RemoteWrite.HTTPAuth, remoteSecrets)
		args = buildHeadersArg("remoteWrite.headers", args, cr.Spec.RemoteWrite.HTTPAuth.Headers)
		if cr.Spec.RemoteWrite.Concurrency != nil {
			args = append(args, fmt.Sprintf("-remoteWrite.concurrency=%d", *cr.Spec.RemoteWrite.Concurrency))
		}
		if cr.Spec.RemoteWrite.FlushInterval != nil {
			args = append(args, fmt.Sprintf("-remoteWrite.flushInterval=%s", *cr.Spec.RemoteWrite.FlushInterval))
		}
		if cr.Spec.RemoteWrite.MaxBatchSize != nil {
			args = append(args, fmt.Sprintf("-remoteWrite.maxBatchSize=%d", *cr.Spec.RemoteWrite.MaxBatchSize))
		}
		if cr.Spec.RemoteWrite.MaxQueueSize != nil {
			args = append(args, fmt.Sprintf("-remoteWrite.maxQueueSize=%d", *cr.Spec.RemoteWrite.MaxQueueSize))
		}
		if cr.Spec.RemoteWrite.HTTPAuth.TLSConfig != nil {
			tlsConf := cr.Spec.RemoteWrite.HTTPAuth.TLSConfig
			args = tlsConf.AsArgs(args, remoteWriteKey, pathPrefix)
		}
	}
	for k, v := range cr.Spec.ExternalLabels {
		args = append(args, fmt.Sprintf("-external.label=%s=%s", k, v))
	}

	if cr.Spec.RemoteRead != nil {
		args = append(args, fmt.Sprintf("-remoteRead.url=%s", cr.Spec.RemoteRead.URL))
		args = buildVMAlertAuthArgs(args, remoteReadKey, cr.Spec.RemoteRead.HTTPAuth, remoteSecrets)
		args = buildHeadersArg("remoteRead.headers", args, cr.Spec.RemoteRead.HTTPAuth.Headers)
		if cr.Spec.RemoteRead.Lookback != nil {
			args = append(args, fmt.Sprintf("-remoteRead.lookback=%s", *cr.Spec.RemoteRead.Lookback))
		}
		if cr.Spec.RemoteRead.HTTPAuth.TLSConfig != nil {
			tlsConf := cr.Spec.RemoteRead.HTTPAuth.TLSConfig
			args = tlsConf.AsArgs(args, remoteReadKey, pathPrefix)
		}

	}
	if cr.Spec.EvaluationInterval != "" {
		args = append(args, fmt.Sprintf("-evaluationInterval=%s", cr.Spec.EvaluationInterval))
	}
	if cr.Spec.LogLevel != "" {
		args = append(args, fmt.Sprintf("-loggerLevel=%s", cr.Spec.LogLevel))
	}
	if cr.Spec.LogFormat != "" {
		args = append(args, fmt.Sprintf("-loggerFormat=%s", cr.Spec.LogFormat))
	}

	for _, cm := range ruleConfigMapNames {
		args = append(args, fmt.Sprintf("-rule=%q", path.Join(vmAlertConfigDir, cm, "*.yaml")))
	}

	args = append(args, fmt.Sprintf("-httpListenAddr=:%s", cr.Spec.Port))

	for _, rulePath := range cr.Spec.RulePath {
		args = append(args, fmt.Sprintf("-rule=%q", rulePath))
	}
	if len(cr.Spec.ExtraEnvs) > 0 {
		args = append(args, "-envflag.enable=true")
	}

	args = cr.Spec.License.MaybeAddToArgs(args, SecretsDir)

	args = addExtraArgsOverrideDefaults(args, cr.Spec.ExtraArgs, "-")
	sort.Strings(args)
	return args
}

type authSecret struct {
	bearerValue string
	*BasicAuthCredentials
	*oauthCreds
}

func loadVMAlertRemoteSecrets(
	ctx context.Context,
	rclient client.Client,
	cr *victoriametricsv1beta1.VMAlert,
) (map[string]*authSecret, error) {
	datasource := cr.Spec.Datasource
	remoteWrite := cr.Spec.RemoteWrite
	remoteRead := cr.Spec.RemoteRead
	ns := cr.Namespace

	nsSecretCache := make(map[string]*corev1.Secret)
	nsCMCache := make(map[string]*corev1.ConfigMap)
	authSecretsBySource := make(map[string]*authSecret)
	loadHTTPAuthSecrets := func(ctx context.Context, rclient client.Client, ns string, httpAuth victoriametricsv1beta1.HTTPAuth) (*authSecret, error) {
		var as authSecret
		if httpAuth.BasicAuth != nil {
			credentials, err := loadBasicAuthSecret(ctx, rclient, cr.Namespace, httpAuth.BasicAuth)
			if err != nil {
				return nil, fmt.Errorf("could not load basicAuth config. %w", err)
			}
			as.BasicAuthCredentials = &credentials
		}
		if httpAuth.BearerAuth != nil && httpAuth.BearerAuth.TokenSecret != nil {
			token, err := getCredFromSecret(ctx, rclient, cr.Namespace, httpAuth.BearerAuth.TokenSecret, buildCacheKey(ns, httpAuth.BearerAuth.TokenSecret.Name), nsSecretCache)
			if err != nil {
				return nil, fmt.Errorf("cannot load bearer auth token: %w", err)
			}
			as.bearerValue = token
		}
		if httpAuth.OAuth2 != nil {
			oauth2, err := loadOAuthSecrets(ctx, rclient, httpAuth.OAuth2, cr.Namespace, nsSecretCache, nsCMCache)
			if err != nil {
				return nil, fmt.Errorf("cannot load oauth2 creds err: %w", err)
			}
			as.oauthCreds = oauth2
		}
		return &as, nil
	}
	for i, notifier := range cr.Spec.Notifiers {
		as, err := loadHTTPAuthSecrets(ctx, rclient, ns, notifier.HTTPAuth)
		if err != nil {
			return nil, err
		}
		authSecretsBySource[buildNotifierKey(i)] = as
	}
	// load basic auth for datasource configuration
	as, err := loadHTTPAuthSecrets(ctx, rclient, ns, datasource.HTTPAuth)
	if err != nil {
		return nil, fmt.Errorf("could not generate basicAuth for datasource config. %w", err)
	}
	authSecretsBySource[datasourceKey] = as

	// load basic auth for remote write configuration
	if remoteWrite != nil {
		as, err := loadHTTPAuthSecrets(ctx, rclient, ns, remoteWrite.HTTPAuth)
		if err != nil {
			return nil, fmt.Errorf("could not generate basicAuth for datasource config. %w", err)
		}
		authSecretsBySource[remoteWriteKey] = as
	}
	// load basic auth for remote write configuration
	if remoteRead != nil {
		as, err := loadHTTPAuthSecrets(ctx, rclient, ns, remoteRead.HTTPAuth)
		if err != nil {
			return nil, fmt.Errorf("could not generate basicAuth for datasource config. %w", err)
		}
		authSecretsBySource[remoteReadKey] = as
	}
	return authSecretsBySource, nil
}

func CreateOrUpdateTlsAssetsForVMAlert(ctx context.Context, cr *victoriametricsv1beta1.VMAlert, rclient client.Client) error {
	assets, err := loadTLSAssetsForVMAlert(ctx, rclient, cr)
	if err != nil {
		return fmt.Errorf("cannot load tls assets: %w", err)
	}

	tlsAssetsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cr.TLSAssetName(),
			Labels:          cr.AllLabels(),
			Annotations:     cr.AnnotationsFiltered(),
			OwnerReferences: cr.AsOwner(),
			Namespace:       cr.Namespace,
			Finalizers:      []string{victoriametricsv1beta1.FinalizerName},
		},
		Data: map[string][]byte{},
	}

	for key, asset := range assets {
		tlsAssetsSecret.Data[key] = []byte(asset)
	}
	currentAssetSecret := &corev1.Secret{}
	err = rclient.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: tlsAssetsSecret.Name}, currentAssetSecret)
	if err != nil {
		if errors.IsNotFound(err) {
			return rclient.Create(ctx, tlsAssetsSecret)
		}
		return fmt.Errorf("cannot get existing tls secret: %s, for vmalert: %s, err: %w", tlsAssetsSecret.Name, cr.Name, err)
	}
	for annotation, value := range currentAssetSecret.Annotations {
		tlsAssetsSecret.Annotations[annotation] = value
	}
	tlsAssetsSecret.Annotations = labels.Merge(currentAssetSecret.Annotations, tlsAssetsSecret.Annotations)
	tlsAssetsSecret.Finalizers = victoriametricsv1beta1.MergeFinalizers(currentAssetSecret, victoriametricsv1beta1.FinalizerName)
	return rclient.Update(ctx, tlsAssetsSecret)
}

func loadTLSAssetsForVMAlert(ctx context.Context, rclient client.Client, cr *victoriametricsv1beta1.VMAlert) (map[string]string, error) {
	assets := map[string]string{}
	nsSecretCache := make(map[string]*corev1.Secret)
	nsConfigMapCache := make(map[string]*corev1.ConfigMap)
	tlsConfigs := []*victoriametricsv1beta1.TLSConfig{}

	for _, notifier := range cr.Spec.Notifiers {
		if notifier.HTTPAuth.TLSConfig != nil {
			tlsConfigs = append(tlsConfigs, notifier.HTTPAuth.TLSConfig)
		}
	}
	if cr.Spec.RemoteRead != nil && cr.Spec.RemoteRead.HTTPAuth.TLSConfig != nil {
		tlsConfigs = append(tlsConfigs, cr.Spec.RemoteRead.HTTPAuth.TLSConfig)
	}
	if cr.Spec.RemoteWrite != nil && cr.Spec.RemoteWrite.HTTPAuth.TLSConfig != nil {
		tlsConfigs = append(tlsConfigs, cr.Spec.RemoteWrite.HTTPAuth.TLSConfig)
	}
	if cr.Spec.Datasource.HTTPAuth.TLSConfig != nil {
		tlsConfigs = append(tlsConfigs, cr.Spec.Datasource.HTTPAuth.TLSConfig)
	}

	for _, rw := range tlsConfigs {
		prefix := cr.Namespace + "/"
		secretSelectors := map[string]*corev1.SecretKeySelector{}
		configMapSelectors := map[string]*corev1.ConfigMapKeySelector{}
		selectorKey := rw.CA.BuildSelectorWithPrefix(prefix)
		switch {
		case rw.CA.Secret != nil:
			secretSelectors[selectorKey] = rw.CA.Secret
		case rw.CA.ConfigMap != nil:
			configMapSelectors[selectorKey] = rw.CA.ConfigMap
		}
		selectorKey = rw.Cert.BuildSelectorWithPrefix(prefix)

		switch {
		case rw.Cert.Secret != nil:
			secretSelectors[selectorKey] = rw.Cert.Secret

		case rw.Cert.ConfigMap != nil:
			configMapSelectors[selectorKey] = rw.Cert.ConfigMap
		}
		if rw.KeySecret != nil {
			secretSelectors[prefix+rw.KeySecret.Name+"/"+rw.KeySecret.Key] = rw.KeySecret
		}
		for key, selector := range secretSelectors {
			asset, err := getCredFromSecret(
				ctx,
				rclient,
				cr.Namespace,
				selector,
				key,
				nsSecretCache,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"failed to extract endpoint tls asset for vmservicescrape %s from secret %s and key %s in namespace %s",
					cr.Name, selector.Name, selector.Key, cr.Namespace,
				)
			}

			assets[rw.BuildAssetPath(cr.Namespace, selector.Name, selector.Key)] = asset
		}

		for key, selector := range configMapSelectors {
			asset, err := getCredFromConfigMap(
				ctx,
				rclient,
				cr.Namespace,
				*selector,
				key,
				nsConfigMapCache,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"failed to extract endpoint tls asset for vmservicescrape %v from configmap %v and key %v in namespace %v",
					cr.Name, selector.Name, selector.Key, cr.Namespace,
				)
			}
			assets[rw.BuildAssetPath(cr.Namespace, selector.Name, selector.Key)] = asset
		}
	}

	return assets, nil
}

func BuildNotifiersArgs(cr *victoriametricsv1beta1.VMAlert, ntBasicAuth map[string]*authSecret) []string {
	var finalArgs []string
	var notifierArgs []remoteFlag
	notifierTargets := cr.Spec.Notifiers

	if len(notifierTargets) == 0 && cr.Spec.NotifierConfigRef != nil {
		return append(finalArgs, fmt.Sprintf("-notifier.config=%s/%s", notifierConfigMountPath, cr.Spec.NotifierConfigRef.Key))
	}

	url := remoteFlag{flagSetting: "-notifier.url=", isNotNull: true}
	authUser := remoteFlag{flagSetting: "-notifier.basicAuth.username="}
	authPasswordFile := remoteFlag{flagSetting: "-notifier.basicAuth.passwordFile="}
	tlsCAs := remoteFlag{flagSetting: "-notifier.tlsCAFile="}
	tlsCerts := remoteFlag{flagSetting: "-notifier.tlsCertFile="}
	tlsKeys := remoteFlag{flagSetting: "-notifier.tlsKeyFile="}
	tlsServerName := remoteFlag{flagSetting: "-notifier.tlsServerName="}
	tlsInSecure := remoteFlag{flagSetting: "-notifier.tlsInsecureSkipVerify="}
	headers := remoteFlag{flagSetting: "-notifier.headers="}
	bearerTokenPath := remoteFlag{flagSetting: "-notifier.bearerTokenFile="}
	oauth2SecretFile := remoteFlag{flagSetting: "-notifier.oauth2.clientSecretFile="}
	oauth2ClientID := remoteFlag{flagSetting: "-notifier.oauth2.clientID="}
	oauth2Scopes := remoteFlag{flagSetting: "-notifier.oauth2.scopes="}
	oauth2TokenURL := remoteFlag{flagSetting: "-notifier.oauth2.tokenUrl="}

	pathPrefix := path.Join(tlsAssetsDir, cr.Namespace)

	for i, nt := range notifierTargets {

		url.flagSetting += fmt.Sprintf("%s,", nt.URL)

		var caPath, certPath, keyPath, ServerName string
		var inSecure bool
		ntTls := nt.HTTPAuth.TLSConfig
		if ntTls != nil {
			if ntTls.CAFile != "" {
				caPath = ntTls.CAFile
			} else if ntTls.CA.Name() != "" {
				caPath = ntTls.BuildAssetPath(pathPrefix, ntTls.CA.Name(), ntTls.CA.Key())
			}
			if caPath != "" {
				tlsCAs.isNotNull = true
			}
			if ntTls.CertFile != "" {
				certPath = ntTls.CertFile
			} else if ntTls.Cert.Name() != "" {
				certPath = ntTls.BuildAssetPath(pathPrefix, ntTls.Cert.Name(), ntTls.Cert.Key())
			}
			if certPath != "" {
				tlsCerts.isNotNull = true
			}
			if ntTls.KeyFile != "" {
				keyPath = ntTls.KeyFile
			} else if ntTls.KeySecret != nil {
				keyPath = ntTls.BuildAssetPath(pathPrefix, ntTls.KeySecret.Name, ntTls.KeySecret.Key)
			}
			if keyPath != "" {
				tlsKeys.isNotNull = true
			}
			if ntTls.InsecureSkipVerify {
				tlsInSecure.isNotNull = true
				inSecure = true
			}
			if ntTls.ServerName != "" {
				ServerName = ntTls.ServerName
				tlsServerName.isNotNull = true
			}
		}
		tlsCAs.flagSetting += fmt.Sprintf("%s,", caPath)
		tlsCerts.flagSetting += fmt.Sprintf("%s,", certPath)
		tlsKeys.flagSetting += fmt.Sprintf("%s,", keyPath)
		tlsServerName.flagSetting += fmt.Sprintf("%s,", ServerName)
		tlsInSecure.flagSetting += fmt.Sprintf("%v,", inSecure)
		var headerFlagValue string
		if len(nt.HTTPAuth.Headers) > 0 {
			for _, headerKV := range nt.HTTPAuth.Headers {
				headerFlagValue += headerKV + "^^"
			}
			headers.isNotNull = true
		}
		headerFlagValue = strings.TrimSuffix(headerFlagValue, "^^")
		headers.flagSetting += fmt.Sprintf("%s,", headerFlagValue)
		var user, passFile string
		s := ntBasicAuth[buildNotifierKey(i)]
		if nt.HTTPAuth.BasicAuth != nil {
			if s == nil {
				panic("secret for basic notifier cannot be nil")
			}
			authUser.isNotNull = true
			user = s.username
			if len(s.password) > 0 {
				passFile = path.Join(vmalertConfigSecretsDir, buildRemoteSecretKey(buildNotifierKey(i), basicAuthPasswordKey))
				authPasswordFile.isNotNull = true
			}
			if len(nt.HTTPAuth.BasicAuth.PasswordFile) > 0 {
				passFile = nt.HTTPAuth.BasicAuth.PasswordFile
				authPasswordFile.isNotNull = true
			}
		}
		authUser.flagSetting += fmt.Sprintf("\"%s\",", strings.ReplaceAll(user, `"`, `\"`))
		authPasswordFile.flagSetting += fmt.Sprintf("%s,", passFile)

		var tokenPath string
		if nt.HTTPAuth.BearerAuth != nil {
			if len(nt.HTTPAuth.BearerAuth.TokenFilePath) > 0 {
				bearerTokenPath.isNotNull = true
				tokenPath = nt.HTTPAuth.BearerAuth.TokenFilePath
			} else if len(s.bearerValue) > 0 {
				bearerTokenPath.isNotNull = true
				tokenPath = path.Join(vmalertConfigSecretsDir, buildRemoteSecretKey(buildNotifierKey(i), bearerTokenKey))
			}
		}
		bearerTokenPath.flagSetting += fmt.Sprintf("%s,", tokenPath)
		var scopes, tokenURL, secretFile, clientID string
		if nt.OAuth2 != nil {
			if s == nil {
				panic("secret for oauth2 notifier cannot be nil")
			}
			if len(nt.OAuth2.Scopes) > 0 {
				oauth2Scopes.isNotNull = true
				scopes = strings.Join(nt.OAuth2.Scopes, ",")
			}
			if len(nt.OAuth2.TokenURL) > 0 {
				oauth2TokenURL.isNotNull = true
				tokenURL = nt.OAuth2.TokenURL
			}
			clientID = s.clientID
			oauth2ClientID.isNotNull = true
			if len(s.clientSecret) > 0 {
				oauth2SecretFile.isNotNull = true
				secretFile = path.Join(vmalertConfigSecretsDir, buildRemoteSecretKey(buildNotifierKey(i), oauth2SecretKey))
			} else {
				oauth2SecretFile.isNotNull = true
				secretFile = nt.OAuth2.ClientSecretFile
			}
		}
		oauth2Scopes.flagSetting += fmt.Sprintf("%s,", scopes)
		oauth2TokenURL.flagSetting += fmt.Sprintf("%s,", tokenURL)
		oauth2ClientID.flagSetting += fmt.Sprintf("%s,", clientID)
		oauth2SecretFile.flagSetting += fmt.Sprintf("%s,", secretFile)
	}
	notifierArgs = append(notifierArgs, url, authUser, authPasswordFile)
	notifierArgs = append(notifierArgs, tlsServerName, tlsKeys, tlsCerts, tlsCAs, tlsInSecure, headers, bearerTokenPath)
	notifierArgs = append(notifierArgs, oauth2SecretFile, oauth2ClientID, oauth2Scopes, oauth2TokenURL)

	for _, remoteArgType := range notifierArgs {
		if remoteArgType.isNotNull {
			finalArgs = append(finalArgs, strings.TrimSuffix(remoteArgType.flagSetting, ","))
		}
	}

	return finalArgs
}
