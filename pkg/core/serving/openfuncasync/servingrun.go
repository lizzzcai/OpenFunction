package openfuncasync

import (
	"context"
	"fmt"
	"strings"

	openfunctioncontext "github.com/OpenFunction/functions-framework-go/openfunction-context"
	jsoniter "github.com/json-iterator/go"

	"github.com/openfunction/pkg/util"

	componentsv1alpha1 "github.com/dapr/dapr/pkg/apis/components/v1alpha1"
	subscriptionsv1alpha1 "github.com/dapr/dapr/pkg/apis/subscriptions/v1alpha1"
	"github.com/go-logr/logr"
	kedav1alpha1 "github.com/kedacore/keda/v2/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	openfunction "github.com/openfunction/apis/core/v1alpha1"
	"github.com/openfunction/pkg/core"
)

const (
	servingLabel = "openfunction.io/serving"

	workloadName     = "OpenFuncAsync/workload"
	serviceName      = "OpenFuncAsync/service"
	scalerName       = "OpenFuncAsync/scaler"
	componentName    = "OpenFuncAsync/component"
	subscriptionName = "OpenFuncAsync/subscription"
)

type servingRun struct {
	client.Client
	ctx    context.Context
	log    logr.Logger
	scheme *runtime.Scheme
}

func NewServingRun(ctx context.Context, c client.Client, scheme *runtime.Scheme, log logr.Logger) core.ServingRun {
	return &servingRun{
		c,
		ctx,
		log.WithName("OpenFuncAsync"),
		scheme,
	}
}

func (r *servingRun) Run(s *openfunction.Serving) error {

	log := r.log.WithName("Run")

	if s.Spec.OpenFuncAsync == nil {
		return fmt.Errorf("OpenFuncAsync config must not be nil when using OpenFuncAsync runtime")
	}

	if err := r.clean(s); err != nil {
		log.Error(err, "Failed to Clean", "name", s.Name, "namespace", s.Namespace)
		return err
	}

	workload := r.generateWorkload(s)
	if err := controllerutil.SetControllerReference(s, workload, r.scheme); err != nil {
		log.Error(err, "Failed to SetControllerReference", "name", s.Name, "namespace", s.Namespace)
		return err
	}

	if err := r.Create(r.ctx, workload); err != nil {
		log.Error(err, "Failed to create workload", "name", s.Name, "namespace", s.Namespace)
		return err
	}

	log.V(1).Info("Workload created", "name", workload.GetName(), "namespace", workload.GetNamespace())

	s.Status.ResourceRef = make(map[string]string)
	s.Status.ResourceRef[workloadName] = workload.GetName()

	if err := r.createScaler(s, workload); err != nil {
		log.Error(err, "Failed to create keda triggers", "name", s.Name, "namespace", s.Namespace)
		return err
	}

	if err := r.createService(s, workload); err != nil {
		log.Error(err, "Failed to create Service", "name", s.Name, "namespace", s.Namespace)
		return err
	}

	if err := r.createOrUpdateComponents(s); err != nil {
		log.Error(err, "Failed to create dapr components", "name", s.Name, "namespace", s.Namespace)
		return err
	}

	if err := r.createOrUpdateSubscriptions(s); err != nil {
		log.Error(err, "Failed to create dapr subscriptions", "name", s.Name, "namespace", s.Namespace)
		return err
	}

	return nil
}

func (r *servingRun) clean(s *openfunction.Serving) error {
	log := r.log.WithName("Clean")

	list := func(lists []client.ObjectList) error {
		for _, list := range lists {
			if err := r.List(r.ctx, list, client.InNamespace(s.Namespace), client.MatchingLabels{servingLabel: s.Name}); err != nil {
				return err
			}
		}

		return nil
	}

	deleteObj := func(obj client.Object) error {
		if strings.HasPrefix(obj.GetName(), s.Name) {
			if err := r.Delete(context.Background(), obj); util.IgnoreNotFound(err) != nil {
				return err
			}
			log.V(1).Info("Delete", "namespace", s.Namespace, "name", obj.GetName())
		}

		return nil
	}

	jobList := &batchv1.JobList{}
	deploymentList := &appsv1.DeploymentList{}
	statefulSetList := &appsv1.StatefulSetList{}
	scalerJobList := &kedav1alpha1.ScaledJobList{}
	scaledObjectList := &kedav1alpha1.ScaledObjectList{}
	serviceList := &corev1.ServiceList{}

	if err := list([]client.ObjectList{jobList, deploymentList, statefulSetList, scalerJobList, scaledObjectList, serviceList}); err != nil {
		return err
	}

	for _, item := range jobList.Items {
		if err := deleteObj(&item); err != nil {
			return err
		}
	}

	for _, item := range deploymentList.Items {
		if err := deleteObj(&item); err != nil {
			return err
		}
	}

	for _, item := range statefulSetList.Items {
		if err := deleteObj(&item); err != nil {
			return err
		}
	}

	for _, item := range scalerJobList.Items {
		if err := deleteObj(&item); err != nil {
			return err
		}
	}

	for _, item := range scaledObjectList.Items {
		if err := deleteObj(&item); err != nil {
			return err
		}
	}

	for _, item := range serviceList.Items {
		if err := deleteObj(&item); err != nil {
			return err
		}
	}

	return nil
}

func (r *servingRun) generateWorkload(s *openfunction.Serving) client.Object {

	labels := map[string]string{
		"openfunction.io/managed": "true",
		servingLabel:              s.Name,
		"runtime":                 string(openfunction.OpenFuncAsync),
	}

	selector := &metav1.LabelSelector{
		MatchLabels: labels,
	}

	var replicas int32 = 1
	if s.Spec.OpenFuncAsync.Keda != nil &&
		s.Spec.OpenFuncAsync.Keda.ScaledObject != nil &&
		s.Spec.OpenFuncAsync.Keda.ScaledObject.MinReplicaCount != nil {
		replicas = *s.Spec.OpenFuncAsync.Keda.ScaledObject.MinReplicaCount
	}

	var port int32 = 8080
	if s.Spec.Port != nil {
		port = *s.Spec.Port
	}

	annotations := make(map[string]string)
	annotations["dapr.io/enabled"] = "true"
	annotations["dapr.io/app-id"] = fmt.Sprintf("%s-%s", strings.TrimSuffix(s.Name, "-serving"), s.Namespace)
	annotations["dapr.io/log-as-json"] = "true"
	if s.Spec.OpenFuncAsync.Dapr != nil {
		for k, v := range s.Spec.OpenFuncAsync.Dapr.Annotations {
			annotations[k] = v
		}
	}

	// The dapr protocol must equal to the protocol of function framework.
	annotations["dapr.io/app-protocol"] = "grpc"
	// The dapr port must equal the function port.
	annotations["dapr.io/app-port"] = fmt.Sprintf("%d", port)

	spec := s.Spec.Template
	if spec == nil {
		spec = &corev1.PodSpec{}
	}

	if s.Spec.ImageCredentials != nil {
		spec.ImagePullSecrets = append(spec.ImagePullSecrets, *s.Spec.ImageCredentials)
	}

	var container *corev1.Container
	for index := range spec.Containers {
		if spec.Containers[index].Name == core.FunctionContainer {
			container = &spec.Containers[index]
		}
	}

	appended := false
	if container == nil {
		container = &corev1.Container{
			Name:            core.FunctionContainer,
			ImagePullPolicy: corev1.PullIfNotPresent,
		}
		appended = true
	}

	container.Image = s.Spec.Image

	container.Ports = append(container.Ports, corev1.ContainerPort{
		Name:          core.FunctionPort,
		ContainerPort: port,
		Protocol:      corev1.ProtocolTCP,
	})

	container.Env = append(container.Env, corev1.EnvVar{
		Name:  "FUNC_CONTEXT",
		Value: createFunctionContext(s),
	})

	if s.Spec.Params != nil {
		for k, v := range s.Spec.Params {
			container.Env = append(container.Env, corev1.EnvVar{
				Name:  k,
				Value: v,
			})
		}
	}

	if appended {
		spec.Containers = append(spec.Containers, *container)
	}

	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: annotations,
			Labels:      labels,
		},
		Spec: *spec,
	}

	objectMeta := metav1.ObjectMeta{
		GenerateName: fmt.Sprintf("%s-%s-", s.Name, strings.ReplaceAll(*s.Spec.Version, ".", "")),
		Namespace:    s.Namespace,
		Labels:       labels,
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: objectMeta,
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: selector,
			Template: template,
		},
	}

	statefulset := &appsv1.StatefulSet{
		ObjectMeta: objectMeta,
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: selector,
			Template: template,
		},
	}

	job := &batchv1.Job{
		ObjectMeta: objectMeta,
		Spec: batchv1.JobSpec{
			Template: template,
		},
	}

	job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
	if s.Spec.OpenFuncAsync.Keda != nil &&
		s.Spec.OpenFuncAsync.Keda.ScaledJob != nil &&
		s.Spec.OpenFuncAsync.Keda.ScaledJob.RestartPolicy != nil {
		job.Spec.Template.Spec.RestartPolicy = *s.Spec.OpenFuncAsync.Keda.ScaledJob.RestartPolicy
	}

	keda := s.Spec.OpenFuncAsync.Keda
	// By default, use deployment to running the function.
	if keda == nil || (keda.ScaledJob == nil && keda.ScaledObject == nil) {
		return deploy
	} else {
		if keda.ScaledJob != nil {
			return job
		} else {
			if keda.ScaledObject.WorkloadType == "StatefulSet" {
				return statefulset
			} else {
				return deploy
			}
		}
	}
}

func (r *servingRun) createScaler(s *openfunction.Serving, workload runtime.Object) error {
	log := r.log.WithName("CreateKedaScaler")

	keda := s.Spec.OpenFuncAsync.Keda
	if keda == nil || (keda.ScaledJob == nil && keda.ScaledObject == nil) {
		return nil
	}

	var obj client.Object
	if keda.ScaledJob != nil {
		ref, err := r.getJobTargetRef(workload)
		if err != nil {
			return err
		}

		scaledJob := keda.ScaledJob
		obj = &kedav1alpha1.ScaledJob{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: fmt.Sprintf("%s-scaler-", s.Name),
				Namespace:    s.Namespace,
				Labels: map[string]string{
					"openfunction.io/managed": "true",
					servingLabel:              s.Name,
					"runtime":                 string(openfunction.OpenFuncAsync),
				},
			},
			Spec: kedav1alpha1.ScaledJobSpec{
				JobTargetRef:               ref,
				PollingInterval:            scaledJob.PollingInterval,
				SuccessfulJobsHistoryLimit: scaledJob.SuccessfulJobsHistoryLimit,
				FailedJobsHistoryLimit:     scaledJob.FailedJobsHistoryLimit,
				EnvSourceContainerName:     core.FunctionContainer,
				MaxReplicaCount:            scaledJob.MaxReplicaCount,
				ScalingStrategy:            scaledJob.ScalingStrategy,
				Triggers:                   scaledJob.Triggers,
			},
		}
	} else {
		ref, err := r.getObjectTargetRef(workload)
		if err != nil {
			return err
		}

		scaledObject := keda.ScaledObject
		obj = &kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: fmt.Sprintf("%s-scaler-", s.Name),
				Namespace:    s.Namespace,
				Labels: map[string]string{
					"openfunction.io/managed": "true",
					servingLabel:              s.Name,
					"runtime":                 string(openfunction.OpenFuncAsync),
				},
			},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef:  ref,
				PollingInterval: scaledObject.PollingInterval,
				CooldownPeriod:  scaledObject.CooldownPeriod,
				MinReplicaCount: scaledObject.MinReplicaCount,
				MaxReplicaCount: scaledObject.MaxReplicaCount,
				Advanced:        scaledObject.Advanced,
				Triggers:        scaledObject.Triggers,
			},
		}
	}

	if err := controllerutil.SetControllerReference(s, obj, r.scheme); err != nil {
		log.Error(err, "Failed to SetControllerReference", "name", s.Name, "namespace", s.Namespace)
		return err
	}

	if err := r.Create(r.ctx, obj); err != nil {
		log.Error(err, "Failed to create keda scaler", "name", s.Name, "namespace", s.Namespace)
		return err
	}

	s.Status.ResourceRef[scalerName] = obj.GetName()

	log.V(1).Info("Keda scaler Created", "serving", obj.GetName(), "namespace", obj.GetNamespace())
	return nil
}

func (r *servingRun) getJobTargetRef(workload runtime.Object) (*batchv1.JobSpec, error) {

	job, ok := workload.(*batchv1.Job)
	if !ok {
		return nil, fmt.Errorf("%s", "Workload is not job")
	}

	ref := job.DeepCopy().Spec
	return &ref, nil
}

func (r *servingRun) getObjectTargetRef(workload runtime.Object) (*kedav1alpha1.ScaleTarget, error) {

	accessor, _ := meta.Accessor(workload)
	ref := &kedav1alpha1.ScaleTarget{
		Name:                   accessor.GetName(),
		EnvSourceContainerName: core.FunctionContainer,
	}

	switch workload.(type) {
	case *appsv1.Deployment:
		ref.Kind = "Deployment"
	case *appsv1.StatefulSet:
		ref.Kind = "StatefulSet"
	default:
		return nil, fmt.Errorf("%s", "Workload is neithor deployment nor statefulSet")
	}

	return ref, nil
}

func (r *servingRun) createService(s *openfunction.Serving, workload client.Object) error {

	log := r.log.WithName("CreateService")

	var port int32 = 8080
	if s.Spec.Port != nil {
		port = *s.Spec.Port
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-svc-", s.Name),
			Namespace:    s.Namespace,
			Labels: map[string]string{
				"openfunction.io/managed": "true",
				servingLabel:              s.Name,
				"runtime":                 string(openfunction.OpenFuncAsync),
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: workload.GetLabels(),
			Type:     corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name: "serving",
					Port: port,
					TargetPort: intstr.IntOrString{
						IntVal: port,
					},
					Protocol: corev1.ProtocolTCP,
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(s, svc, r.scheme); err != nil {
		log.Error(err, "Failed to mutate dapr service", "name", s.Name, "namespace", s.Namespace)
		return err
	}

	if err := r.Create(r.ctx, svc); err != nil {
		log.Error(err, "Failed to create service", "name", s.Name, "namespace", s.Namespace)
		return err
	}

	s.Status.ResourceRef[serviceName] = svc.Name

	log.V(1).Info("Service created", "name", svc.Name, "namespace", svc.Namespace)
	return nil
}

func (r *servingRun) createOrUpdateComponents(s *openfunction.Serving) error {
	log := r.log.WithName("CreateOrUpdateDaprComponents")

	dapr := s.Spec.OpenFuncAsync.Dapr
	if dapr == nil {
		return nil
	}

	value := ""
	for _, dc := range dapr.Components {
		component := &componentsv1alpha1.Component{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dc.Name,
				Namespace: s.Namespace,
			},
		}

		if _, err := controllerutil.CreateOrUpdate(r.ctx, r.Client, component, r.mutateComponent(component, dc.ComponentSpec, s)); err != nil {
			log.Error(err, "Failed to CreateOrUpdate dapr component", "namespace", s.Namespace, "serving", s.Name, "component", dc.Name)
			return err
		}

		value = fmt.Sprintf("%s%s,", value, component.Name)
		log.V(1).Info("Component CreateOrUpdate", "namespace", s.Namespace, "serving", s.Name, "component", dc.Name)
	}

	if value != "" {
		s.Status.ResourceRef[componentName] = strings.TrimSuffix(value, ",")
	}

	return nil
}

func (r *servingRun) mutateComponent(component *componentsv1alpha1.Component, spec componentsv1alpha1.ComponentSpec, s *openfunction.Serving) controllerutil.MutateFn {

	return func() error {
		component.Spec = spec
		return controllerutil.SetControllerReference(s, component, r.scheme)
	}
}

func (r *servingRun) createOrUpdateSubscriptions(s *openfunction.Serving) error {
	log := r.log.WithName("CreateOrUpdateDaprSubscriptions")

	dapr := s.Spec.OpenFuncAsync.Dapr
	if dapr == nil {
		return nil
	}

	value := ""
	for _, ds := range dapr.Subscriptions {
		subscription := &subscriptionsv1alpha1.Subscription{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ds.Name,
				Namespace: s.Namespace,
			},
		}

		if _, err := controllerutil.CreateOrUpdate(r.ctx, r.Client, subscription, r.mutateSubscription(subscription, &ds, s)); err != nil {
			log.Error(err, "Failed to CreateOrUpdate dapr subscription", "namespace", s.Namespace, "serving", s.Name, "subscription", ds.Name)
			return err
		}

		value = fmt.Sprintf("%s%s,", value, subscription.Name)
		log.V(1).Info("Subscription CreateOrUpdate", "namespace", s.Namespace, "serving", s.Name, "subscription", ds.Name)
	}

	if value != "" {
		s.Status.ResourceRef[subscriptionName] = strings.TrimSuffix(value, ",")
	}
	return nil
}

func (r *servingRun) mutateSubscription(subscription *subscriptionsv1alpha1.Subscription, ds *openfunction.DaprSubscription, s *openfunction.Serving) controllerutil.MutateFn {

	return func() error {
		subscription.Spec = ds.SubscriptionSpec
		subscription.Scopes = ds.Scopes
		return controllerutil.SetControllerReference(s, subscription, r.scheme)
	}
}

func createFunctionContext(s *openfunction.Serving) string {

	rt := openfunctioncontext.Knative
	if s.Spec.Runtime != nil {
		rt = openfunctioncontext.Runtime(*s.Spec.Runtime)
	}

	var port int32 = 8080
	if s.Spec.Port != nil {
		port = *s.Spec.Port
	}

	version := ""
	if s.Spec.Version != nil {
		version = *s.Spec.Version
	}

	fc := openfunctioncontext.OpenFunctionContext{
		Name:    getFunctionName(s),
		Version: version,
		Runtime: rt,
		Port:    fmt.Sprintf("%d", port),
	}

	if s.Spec.OpenFuncAsync != nil && s.Spec.OpenFuncAsync.Dapr != nil {
		dapr := s.Spec.OpenFuncAsync.Dapr

		if dapr.Inputs != nil && len(dapr.Inputs) > 0 {
			input := dapr.Inputs[0]
			fc.Input = openfunctioncontext.Input{
				Name:   input.Name,
				Uri:    getUri(input),
				Params: input.Params,
			}

			if fc.Input.Params == nil {
				fc.Input.Params = map[string]string{
					"type": input.Type,
				}
			}
		}

		if dapr.Outputs != nil && len(dapr.Outputs) > 0 {
			fc.Outputs = make(map[string]*openfunctioncontext.Output)

			for _, o := range dapr.Outputs {
				output := openfunctioncontext.Output{
					Uri:    getUri(o),
					Params: o.Params,
				}

				if output.Params == nil {
					output.Params = map[string]string{
						"type": o.Type,
					}
				}

				fc.Outputs[o.Name] = &output
			}
		}
	}

	bs, _ := jsoniter.Marshal(fc)
	return string(bs)
}

func getUri(io *openfunction.DaprIO) string {
	switch io.Type {
	case string(openfunctioncontext.OpenFuncBinding):
		return io.Name
	case string(openfunctioncontext.OpenFuncTopic):
		return io.Topic
	case string(openfunctioncontext.OpenFuncService):
		return io.MethodName
	default:
		return ""
	}
}

func getFunctionName(s *openfunction.Serving) string {

	return s.Name[:strings.LastIndex(s.Name, "-serving")]
}
