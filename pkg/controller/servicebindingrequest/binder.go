package servicebindingrequest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"gotest.tools/assert/cmp"
	corev1 "k8s.io/api/core/v1"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/redhat-developer/service-binding-operator/pkg/apis/apps/v1alpha1"
	"github.com/redhat-developer/service-binding-operator/pkg/log"
)

var (
	// containersPath logical path to find containers on supported objects
	containersPath = []string{"spec", "template", "spec", "containers"}
	// volumesPath logical path to find volumes on supported objects
	volumesPath = []string{"spec", "template", "spec", "volumes"}
)

// ChangeTriggerEnv hijacking environment in order to trigger a change
const ChangeTriggerEnv = "ServiceBindingOperatorChangeTriggerEnvVar"

// Binder executes the "binding" act of updating different application kinds to use intermediary
// secret. Those secrets should be offered as environment variables.
type Binder struct {
	ctx        context.Context                 // request context
	client     client.Client                   // kubernetes API client
	dynClient  dynamic.Interface               // kubernetes dynamic api client
	sbr        *v1alpha1.ServiceBindingRequest // instantiated service binding request
	volumeKeys []string                        // list of key names used in volume mounts
	logger     *log.Log                        // logger instance
}

var EmptyApplicationSelectorErr = errors.New("application ResourceRef or MatchLabel not found")

// search objects based in Kind/APIVersion, which contain the labels defined in ApplicationSelector.
func (b *Binder) search() (*unstructured.UnstructuredList, error) {
	ns := b.sbr.GetNamespace()
	gvr := schema.GroupVersionResource{
		Group:    b.sbr.Spec.ApplicationSelector.GroupVersionResource.Group,
		Version:  b.sbr.Spec.ApplicationSelector.GroupVersionResource.Version,
		Resource: b.sbr.Spec.ApplicationSelector.GroupVersionResource.Resource,
	}

	var opts metav1.ListOptions

	// If Application name is present
	if b.sbr.Spec.ApplicationSelector.ResourceRef != "" {
		fieldName := make(map[string]string)
		fieldName["metadata.name"] = b.sbr.Spec.ApplicationSelector.ResourceRef
		opts = metav1.ListOptions{
			FieldSelector: fields.Set(fieldName).String(),
		}
	} else if b.sbr.Spec.ApplicationSelector.LabelSelector != nil {
		matchLabels := b.sbr.Spec.ApplicationSelector.LabelSelector.MatchLabels
		opts = metav1.ListOptions{
			LabelSelector: labels.Set(matchLabels).String(),
		}
	} else {
		return nil, EmptyApplicationSelectorErr
	}

	objList, err := b.dynClient.Resource(gvr).Namespace(ns).List(opts)
	if err != nil {
		return nil, err
	}

	// Return fake NotFound error explicitly to ensure requeue when objList(^) is empty.
	if len(objList.Items) == 0 {
		return nil, k8serror.NewNotFound(
			gvr.GroupResource(),
			b.sbr.Spec.ApplicationSelector.GroupVersionResource.Resource,
		)
	}
	return objList, err
}

// extractSpecVolumes based on volume path, extract it unstructured. It can return error on trying
// to find data in informed Unstructured object.
func (b *Binder) extractSpecVolumes(obj *unstructured.Unstructured) ([]interface{}, error) {
	log := b.logger.WithValues("Volumes.NestedPath", volumesPath)
	log.Debug("Reading volumes definitions...")
	volumes, _, err := unstructured.NestedSlice(obj.Object, volumesPath...)
	if err != nil {
		return nil, err
	}
	return volumes, nil
}

// updateSpecVolumes execute the inspection and update "volumes" entries in informed spec.
func (b *Binder) updateSpecVolumes(
	obj *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	volumes, err := b.extractSpecVolumes(obj)
	if err != nil {
		return nil, err
	}

	volumes, err = b.updateVolumes(volumes)
	if err != nil {
		return nil, err
	}
	if err = unstructured.SetNestedSlice(obj.Object, volumes, volumesPath...); err != nil {
		return nil, err
	}
	return obj, nil
}

// removeSpecVolumes based on extract volume subset, removing volume bind volume entry. It can return
// error on navigating though unstructured object, or in the case of having issues to edit
// unstructured resource.
func (b *Binder) removeSpecVolumes(
	obj *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	volumes, err := b.extractSpecVolumes(obj)
	if err != nil {
		return nil, err
	}
	volumes = b.removeVolumes(volumes)
	if err = unstructured.SetNestedSlice(obj.Object, volumes, volumesPath...); err != nil {
		return nil, err
	}
	return obj, nil
}

// updateVolumes inspect informed list assuming as []corev1.Volume, and if binding volume is already
// defined just return the same list, otherwise, appending the binding volume.
func (b *Binder) updateVolumes(volumes []interface{}) ([]interface{}, error) {
	name := b.sbr.GetName()
	log := b.logger
	log.Debug("Checking if binding volume is already defined...")
	for _, v := range volumes {
		volume := v.(corev1.Volume)
		if name == volume.Name {
			log.Debug("Volume is already defined!")
			return volumes, nil
		}
	}

	items := []corev1.KeyToPath{}
	for _, k := range b.volumeKeys {
		items = append(items, corev1.KeyToPath{Key: k, Path: k})
	}

	log.Debug("Appending new volume with items.", "Items", items)
	bindVolume := &corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: name,
				Items:      items,
			},
		},
	}

	// making sure tranforming it back to unstructured before returning
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(bindVolume)
	if err != nil {
		return nil, err
	}
	return append(volumes, u), nil
}

// removeVolumes remove the bind volumes from informed list of unstructured volumes.
func (b *Binder) removeVolumes(volumes []interface{}) []interface{} {
	name := b.sbr.GetName()
	var cleanVolumes []interface{}
	for _, v := range volumes {
		volume := v.(corev1.Volume)
		if name != volume.Name {
			cleanVolumes = append(cleanVolumes, v)
		}
	}
	return cleanVolumes
}

// extractSpecContainers search for
func (b *Binder) extractSpecContainers(obj *unstructured.Unstructured) ([]interface{}, error) {
	log := b.logger.WithValues("Containers.NestedPath", containersPath)

	containers, found, err := unstructured.NestedSlice(obj.Object, containersPath...)
	if err != nil {
		return nil, err
	}
	if !found {
		err = fmt.Errorf("unable to find '%#v' in object kind '%s'", containersPath, obj.GetKind())
		log.Error(err, "is this definition supported by this operator?")
		return nil, err
	}

	return containers, nil
}

// updateSpecContainers extract containers from object, and trigger update.
func (b *Binder) updateSpecContainers(
	obj *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	containers, err := b.extractSpecContainers(obj)
	if err != nil {
		return nil, err
	}
	if containers, err = b.updateContainers(containers); err != nil {
		return nil, err
	}
	if err = unstructured.SetNestedSlice(obj.Object, containers, containersPath...); err != nil {
		return nil, err
	}
	return obj, nil
}

// removeSpecContainers find and edit containers resource subset, removing bind related entries
// from the object. It can return error on extracting data, editing steps and final editing of to be
// returned object.
func (b *Binder) removeSpecContainers(
	obj *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	containers, err := b.extractSpecContainers(obj)
	if err != nil {
		return nil, err
	}
	if containers, err = b.removeContainers(containers); err != nil {
		return nil, err
	}
	if err = unstructured.SetNestedSlice(obj.Object, containers, containersPath...); err != nil {
		return nil, err
	}
	return obj, nil
}

// updateContainers execute the update command per container found.
func (b *Binder) updateContainers(containers []interface{}) ([]interface{}, error) {
	var err error

	for i, container := range containers {
		log := b.logger.WithValues("Obj.Container.Number", i)
		log.Debug("Inspecting container...")

		containers[i], err = b.updateContainer(container)
		if err != nil {
			log.Error(err, "during container update to add binding items.")
			return nil, err
		}
	}

	return containers, nil
}

// removeContainers execute removal of binding related entries in containers.
func (b *Binder) removeContainers(containers []interface{}) ([]interface{}, error) {
	var err error

	for i, container := range containers {
		log := b.logger.WithValues("Obj.Container.Number", i)
		log.Debug("Inspecting container...")

		containers[i], err = b.removeContainer(container)
		if err != nil {
			log.Error(err, "during container update to remove binding items.")
			return nil, err
		}
	}
	return containers, nil
}

// appendEnvVar append a single environment variable onto informed "EnvVar" instance.
func (b *Binder) appendEnvVar(
	envList []corev1.EnvVar,
	envParam string,
	envValue string,
) []corev1.EnvVar {
	var updatedEnvList []corev1.EnvVar

	alreadyPresent := false
	for _, env := range envList {
		if env.Name == envParam {
			env.Value = envValue
			alreadyPresent = true
		}
		updatedEnvList = append(updatedEnvList, env)
	}

	if !alreadyPresent {
		updatedEnvList = append(updatedEnvList, corev1.EnvVar{
			Name:  envParam,
			Value: envValue,
		})
	}
	return updatedEnvList
}

// appendEnvFrom based on secret name and list of EnvFromSource instances, making sure secret is
// part of the list or appended.
func (b *Binder) appendEnvFrom(envList []corev1.EnvFromSource, secret string) []corev1.EnvFromSource {
	for _, env := range envList {
		if env.SecretRef.Name == secret {
			b.logger.Debug("Directive 'envFrom' is already present!")
			// secret name is already referenced
			return envList
		}
	}

	b.logger.Debug("Adding 'envFrom' directive...")
	return append(envList, corev1.EnvFromSource{
		SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: secret,
			},
		},
	})
}

// removeEnvFrom remove bind related entry from slice of "EnvFromSource".
func (b *Binder) removeEnvFrom(envList []corev1.EnvFromSource, secret string) []corev1.EnvFromSource {
	var cleanEnvList []corev1.EnvFromSource
	for _, env := range envList {
		if env.SecretRef.Name != secret {
			cleanEnvList = append(cleanEnvList, env)
		}
	}
	return cleanEnvList
}

// containerFromUnstructured based on informed unstructured corev1.Container, convert it back to the
// original type. It can return errors on the process.
func (b *Binder) containerFromUnstructured(container interface{}) (*corev1.Container, error) {
	c := &corev1.Container{}
	u := container.(map[string]interface{})
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u, c)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// updateContainer execute the update of a single container, adding binding items.
func (b *Binder) updateContainer(container interface{}) (map[string]interface{}, error) {
	c, err := b.containerFromUnstructured(container)
	if err != nil {
		return nil, err
	}

	// effectively binding the application with intermediary secret
	c.EnvFrom = b.appendEnvFrom(c.EnvFrom, b.sbr.GetName())

	// add a special environment variable that is only used to trigger a change in the declaration,
	// attempting to force a side effect (in case of a Deployment, it would result in its Pods to be
	// restarted)
	c.Env = b.appendEnvVar(c.Env, ChangeTriggerEnv, time.Now().Format(time.RFC3339))

	if len(b.volumeKeys) > 0 {
		// and adding volume mount entries
		c.VolumeMounts = b.appendVolumeMounts(c.VolumeMounts)
	}

	return runtime.DefaultUnstructuredConverter.ToUnstructured(c)
}

// removeContainer execute the update of single container to remove binding items.
func (b *Binder) removeContainer(container interface{}) (map[string]interface{}, error) {
	c, err := b.containerFromUnstructured(container)
	if err != nil {
		return nil, err
	}

	// removing intermediary secret, effectively unbinding the application
	c.EnvFrom = b.removeEnvFrom(c.EnvFrom, b.sbr.GetName())

	if len(b.volumeKeys) > 0 {
		// removing volume mount entries
		c.VolumeMounts = b.removeVolumeMounts(c.VolumeMounts)
	}

	return runtime.DefaultUnstructuredConverter.ToUnstructured(c)
}

// appendVolumeMounts append the binding volume in the template level.
func (b *Binder) appendVolumeMounts(volumeMounts []corev1.VolumeMount) []corev1.VolumeMount {
	name := b.sbr.GetName()
	mountPath := b.sbr.Spec.MountPathPrefix
	if mountPath == "" {
		mountPath = "/var/data"
	}

	for _, v := range volumeMounts {
		if name == v.Name {
			return volumeMounts
		}
	}

	return append(volumeMounts, corev1.VolumeMount{
		Name:      name,
		MountPath: mountPath,
	})
}

// removeVolumeMounts from informed slice of corev1.VolumeMount, make sure all binding related
// entries won't be part of returned slice.
func (b *Binder) removeVolumeMounts(volumeMounts []corev1.VolumeMount) []corev1.VolumeMount {
	var cleanVolumeMounts []corev1.VolumeMount
	name := b.sbr.GetName()
	for _, v := range volumeMounts {
		if name != v.Name {
			cleanVolumeMounts = append(cleanVolumeMounts, v)
		}
	}
	return cleanVolumeMounts
}

// nestedMapComparison compares a nested field from two objects.
func nestedMapComparison(a, b *unstructured.Unstructured, fields ...string) (bool, error) {
	var (
		aMap map[string]interface{}
		bMap map[string]interface{}
		aOk  bool
		bOk  bool
		err  error
	)

	if aMap, aOk, err = unstructured.NestedMap(a.Object, fields...); err != nil {
		return false, err
	}

	if bMap, bOk, err = unstructured.NestedMap(b.Object, fields...); err != nil {
		return false, err
	}

	if aOk != bOk {
		return false, nil
	}

	result := cmp.DeepEqual(aMap, bMap)()

	return result.Success(), nil
}

// update the list of objects informed as unstructured, looking for "containers" entry. This method
// loops over each container to inspect "envFrom" and append the intermediary secret, having the same
// name than original ServiceBindingRequest.
func (b *Binder) update(objs *unstructured.UnstructuredList) ([]*unstructured.Unstructured, error) {
	updatedObjs := []*unstructured.Unstructured{}

	for _, obj := range objs.Items {
		// store a copy of the original object to later be used in a comparison
		originalObj := obj.DeepCopy()
		name := obj.GetName()
		log := b.logger.WithValues("Obj.Name", name, "Obj.Kind", obj.GetKind())
		log.Debug("Inspecting object...")

		updatedObj, err := b.updateSpecContainers(&obj)
		if err != nil {
			return nil, err
		}

		if len(b.volumeKeys) > 0 {
			if updatedObj, err = b.updateSpecVolumes(&obj); err != nil {
				return nil, err
			}
		}

		if specsAreEqual, err := nestedMapComparison(originalObj, updatedObj, "spec"); err != nil {
			log.Error(err, "")
			continue
		} else if specsAreEqual {
			continue
		}

		log.Debug("Updating object...")
		if err := b.client.Update(b.ctx, updatedObj); err != nil {
			return nil, err
		}

		log.Debug("Reading back updated object...")
		// reading object back again, to comply with possible modifications
		namespacedName := types.NamespacedName{
			Namespace: updatedObj.GetNamespace(),
			Name:      updatedObj.GetName(),
		}
		if err = b.client.Get(b.ctx, namespacedName, updatedObj); err != nil {
			return nil, err
		}

		updatedObjs = append(updatedObjs, updatedObj)
	}

	return updatedObjs, nil
}

// remove attempts to update each given object without any service binding related information.
func (b *Binder) remove(objs *unstructured.UnstructuredList) error {
	for _, obj := range objs.Items {
		name := obj.GetName()
		logger := b.logger.WithValues("Obj.Name", name, "Obj.Kind", obj.GetKind())
		logger.Debug("Inspecting object...")

		updatedObj, err := b.removeSpecContainers(&obj)
		if err != nil {
			return err
		}

		if len(b.volumeKeys) > 0 {
			if updatedObj, err = b.removeSpecVolumes(&obj); err != nil {
				return err
			}
		}

		logger.Debug("Updating object...")
		if err = b.client.Update(b.ctx, updatedObj); err != nil {
			return err
		}
	}
	return nil
}

// Unbind select objects subject to binding, and proceed with "remove", which will unbind objects.
func (b *Binder) Unbind() error {
	objs, err := b.search()
	if err != nil {
		return err
	}
	return b.remove(objs)
}

// Bind resources to intermediary secret, by searching informed ResourceKind containing the labels
// in ApplicationSelector, and then updating spec.
func (b *Binder) Bind() ([]*unstructured.Unstructured, error) {
	objs, err := b.search()
	if err != nil {
		return nil, err
	}
	return b.update(objs)
}

// NewBinder returns a new Binder instance.
func NewBinder(
	ctx context.Context,
	client client.Client,
	dynClient dynamic.Interface,
	sbr *v1alpha1.ServiceBindingRequest,
	volumeKeys []string,
) *Binder {
	return &Binder{
		ctx:        ctx,
		client:     client,
		dynClient:  dynClient,
		sbr:        sbr,
		volumeKeys: volumeKeys,
		logger:     log.NewLog("binder"),
	}
}
