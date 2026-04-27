package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"
	"time"

	goyaml "github.com/goccy/go-yaml"
	"github.com/google/uuid"
	"github.com/tigrisdata-community/mithras/internal/webhook/webhookconfig"
	"github.com/yokecd/yoke/pkg/flight"
	"github.com/yokecd/yoke/pkg/flight/wasi/k8s"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"

	agentv1alpha1 "github.com/tigrisdata-community/mithras/internal/k8s/agent/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func main() {
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var agent agentv1alpha1.Agent
	if err := yaml.NewYAMLToJSONDecoder(os.Stdin).Decode(&agent); err != nil && err != io.EOF {
		return fmt.Errorf("can't unmarshal input: %w", err)
	}

	agent.Namespace = flight.Namespace()

	if len(agent.Annotations) == 0 {
		agent.Annotations = map[string]string{}
	}

	if len(agent.Labels) == 0 {
		agent.Labels = map[string]string{}
	}
	maps.Copy(agent.Labels, selector(agent))

	credsSecret, err := k8s.Lookup[corev1.Secret](k8s.ResourceIdentifier{
		ApiVersion: "v1",
		Kind:       "Secret",
		Name:       agent.Spec.CredsSecret,
		Namespace:  agent.Namespace,
	})
	if err != nil {
		return fmt.Errorf("can't lookup credentials secret %q: %w", agent.Spec.CredsSecret, err)
	}

	webhookSecret, err := k8s.Lookup[corev1.Secret](k8s.ResourceIdentifier{
		ApiVersion: "v1",
		Kind:       "Secret",
		Name:       agent.Name + "-webhook-secret",
		Namespace:  agent.Namespace,
	})
	if err != nil {
		webhookSecret = makeWebhookSecret(agent)
	}

	var result []any = make([]any, 0)

	cm, err := makeConfigMap(agent, credsSecret)
	if err != nil {
		return fmt.Errorf("can't make configmap: %w", err)
	}

	result = append(result, cm, webhookSecret)
	result = append(result, makeDeployment(agent))
	result = append(result, makeService(agent))
	result = append(result, makeIngress(agent))

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	return enc.Encode(result)
}

// Our selector for our backend application. Independent from the regular labels passed in the backend spec.
func selector(a agentv1alpha1.Agent) map[string]string {
	return map[string]string{"mithras.tigris.sh/agent": a.Name}
}

func makeConfigMap(a agentv1alpha1.Agent, sec *corev1.Secret) (*corev1.ConfigMap, error) {
	prt, err := time.ParseDuration(a.Spec.PerRequestTimeout)
	if err != nil {
		return nil, fmt.Errorf("%w time.ParseDuration(perRequestTimeout: %q): %w", agentv1alpha1.ErrInvalidFormat, a.Spec.PerRequestTimeout, err)
	}

	cfg := webhookconfig.Config{
		AgentName:       a.Name,
		Model:           a.Spec.Model,
		ProviderBaseURL: string(sec.Data["OPENAI_BASE_URL"]),
		SystemPrompt:    a.Spec.SystemPrompt,
		Bucket:          a.Spec.Bucket,
		S3: webhookconfig.S3Config{
			Endpoint:     "https://t3.storage.dev",
			Region:       "auto",
			UsePathStyle: new(false),
		},
		Tools:             a.Spec.Tools,
		PerRequestTimeout: prt,
		ParallelToolCalls: new(true),
	}

	if err := cfg.Valid(); err != nil {
		return nil, fmt.Errorf("can't validate config: %w", err)
	}

	cfgData, err := goyaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("can't marshal config: %w", err)
	}

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.Identifier(),
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        a.Name + "-cfg",
			Namespace:   a.Namespace,
			Labels:      a.Labels,
			Annotations: a.Annotations,
		},
		Data: map[string]string{
			"config.yaml": string(cfgData),
		},
	}, nil
}

func makeDeployment(a agentv1alpha1.Agent) *appsv1.Deployment {
	result := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: appsv1.SchemeGroupVersion.Identifier(),
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        a.Name + "-agent",
			Namespace:   a.Namespace,
			Labels:      a.Labels,
			Annotations: a.Annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(3)),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
			},
			Selector: &metav1.LabelSelector{MatchLabels: selector(a)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: a.Labels},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup: ptr.To[int64](1000),
					},
					Containers: []corev1.Container{
						{
							Name:            "webhookd",
							Image:           "ghcr.io/tigrisdata-community/mithras/webhookd:latest",
							ImagePullPolicy: corev1.PullAlways,
							SecurityContext: &corev1.SecurityContext{
								RunAsUser:                ptr.To[int64](1000),
								RunAsGroup:               ptr.To[int64](1000),
								RunAsNonRoot:             ptr.To(true),
								AllowPrivilegeEscalation: ptr.To(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
								SeccompProfile: &corev1.SeccompProfile{
									Type: corev1.SeccompProfileTypeRuntimeDefault,
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "CONFIG_PATH",
									Value: "/run/secrets/crd-spec/config.yaml",
								},
								{
									Name:  "BIND",
									Value: ":8080",
								},
							},
							EnvFrom: []corev1.EnvFromSource{
								{
									SecretRef: &corev1.SecretEnvSource{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: a.Spec.CredsSecret,
										},
									},
								},
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									Protocol:      corev1.ProtocolTCP,
									ContainerPort: int32(8080),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config",
									MountPath: "/run/secrets/crd-spec",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: a.Name + "-cfg",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	probe := &corev1.Probe{
		InitialDelaySeconds: 3,
		PeriodSeconds:       10,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz",
				Port: intstr.FromInt(8080),
				HTTPHeaders: []corev1.HTTPHeader{
					{
						Name:  "X-Kubernetes",
						Value: "healthcheck",
					},
				},
			},
		},
	}

	result.Spec.Template.Spec.Containers[0].LivenessProbe = probe
	result.Spec.Template.Spec.Containers[0].ReadinessProbe = probe

	return result
}

func makeService(a agentv1alpha1.Agent) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.Identifier(),
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        a.Name,
			Namespace:   a.Namespace,
			Labels:      a.Labels,
			Annotations: a.Annotations,
		},
		Spec: corev1.ServiceSpec{
			Selector: selector(a),
			Type:     corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					Name:       "http",
				},
			},
		},
	}
}

func makeIngress(a agentv1alpha1.Agent) *networkingv1.Ingress {
	annotations := map[string]string{
		"cert-manager.io/cluster-issuer": "letsencrypt-prod",
	}

	maps.Copy(annotations, a.Annotations)

	result := &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{
			APIVersion: networkingv1.SchemeGroupVersion.Identifier(),
			Kind:       "Ingress",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        a.Name,
			Namespace:   a.Namespace,
			Labels:      a.Labels,
			Annotations: annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: new(a.Spec.Ingress.Class),
			TLS: []networkingv1.IngressTLS{
				{
					Hosts:      []string{a.Spec.Ingress.Host},
					SecretName: mkTLSSecretName(a.Spec.Ingress.Host),
				},
			},
			Rules: []networkingv1.IngressRule{
				{
					Host: a.Spec.Ingress.Host,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									PathType: new(networkingv1.PathTypePrefix),
									Path:     "/",
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: a.Name,
											Port: networkingv1.ServiceBackendPort{
												Name: "http",
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	return result
}

func mkTLSSecretName(domain string) string {
	return fmt.Sprintf("%s-public-tls", strings.ReplaceAll(domain, ".", "-"))
}

func makeWebhookSecret(a agentv1alpha1.Agent) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: a.Name + "-webhook-secret",
		},
		StringData: map[string]string{
			"WEBHOOK_SHARED_SECRET": uuid.Must(uuid.NewV7()).String(),
		},
	}
}
