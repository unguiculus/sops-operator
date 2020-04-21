package sopssecret

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"time"
	"unicode"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/craftypath/sops-operator/pkg/sops"

	craftypathv1alpha1 "github.com/craftypath/sops-operator/pkg/apis/craftypath/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const controllerName = "sopssecret-controller"

var log = logf.Log.WithName(controllerName)

type Decryptor interface {
	Decrypt(fileName string, encrypted string) ([]byte, error)
}

// Add creates a new SopsSecret Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileSopsSecret{
		client:    mgr.GetClient(),
		scheme:    mgr.GetScheme(),
		recorder:  mgr.GetEventRecorderFor(controllerName),
		decryptor: &sops.Decryptor{},
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(controllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource SopsSecret
	err = c.Watch(&source.Kind{Type: &craftypathv1alpha1.SopsSecret{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// // Watch for changes to secondary resource Pods and requeue the owner SopsSecret
	// err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
	// 	IsController: true,
	// 	OwnerType:    &craftypathv1alpha1.SopsSecret{},
	// })
	// if err != nil {
	// 	return err
	// }

	return nil
}

// blank assignment to verify that ReconcileSopsSecret implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileSopsSecret{}

// ReconcileSopsSecret reconciles a SopsSecret object
type ReconcileSopsSecret struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client    client.Client
	scheme    *runtime.Scheme
	recorder  record.EventRecorder
	decryptor Decryptor
}

// Reconcile reads that state of the cluster for a SopsSecret object and makes changes based on the state read
// and what is in the SopsSecret.Spec
func (r *ReconcileSopsSecret) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling SopsSecret")

	ctx := context.Background()

	// Fetch the SopsSecret instance
	instance := &craftypathv1alpha1.SopsSecret{}
	if err := r.client.Get(context.TODO(), request.NamespacedName, instance); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
	}

	result, err := ctrl.CreateOrUpdate(ctx, r.client, secret, func() error {
		if !secret.CreationTimestamp.IsZero() {
			if !metav1.IsControlledBy(secret, instance) {
				return fmt.Errorf("secret already exists and not owned by sops-operator")
			}
		}
		if err := r.update(secret, instance); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return r.manageError(instance, err)
	}

	return r.manageSuccess(instance, result)
}

func (r *ReconcileSopsSecret) update(secret *corev1.Secret, sopsSecret *craftypathv1alpha1.SopsSecret) error {
	data := make(map[string][]byte, len(sopsSecret.Spec.StringData))
	for fileName, encryptedContents := range sopsSecret.Spec.StringData {
		decrypted, err := r.decryptor.Decrypt(fileName, encryptedContents)
		if err != nil {
			return err
		}
		buf := make([]byte, base64.StdEncoding.EncodedLen(len(decrypted)))
		base64.StdEncoding.Encode(buf, decrypted)
		data[fileName] = buf
	}

	secret.ObjectMeta.Annotations = sopsSecret.Annotations
	secret.ObjectMeta.Labels = sopsSecret.Labels
	secret.Data = data
	if sopsSecret.Spec.Type != "" {
		secret.Type = sopsSecret.Spec.Type
	}

	if err := ctrl.SetControllerReference(sopsSecret, secret, r.scheme); err != nil {
		return fmt.Errorf("unable to set ownerReference: %w", err)
	}
	return nil
}

func (r *ReconcileSopsSecret) manageError(instance *craftypathv1alpha1.SopsSecret, issue error) (reconcile.Result, error) {
	var retryInterval time.Duration

	r.recorder.Event(instance, "Warning", "ProcessingError", capitalizeFirst(issue.Error()))

	lastUpdate := instance.Status.LastUpdate
	lastStatus := instance.Status.Status

	status := craftypathv1alpha1.SopsSecretStatus{
		LastUpdate: metav1.Now(),
		Reason:     issue.Error(),
		Status:     "Failure",
	}
	instance.Status = status

	if err := r.client.Status().Update(context.Background(), instance); err != nil {
		log.Error(err, "Unable to update status")
		return reconcile.Result{
			RequeueAfter: time.Second,
			Requeue:      true,
		}, nil
	}

	if lastUpdate.IsZero() || lastStatus == "Success" {
		retryInterval = time.Second
	} else {
		retryInterval = status.LastUpdate.Sub(lastUpdate.Time.Round(time.Second))
	}

	return reconcile.Result{
		RequeueAfter: time.Duration(math.Min(float64(retryInterval.Nanoseconds()*2), float64(time.Hour.Nanoseconds()*6))),
		Requeue:      true,
	}, nil
}

func (r *ReconcileSopsSecret) manageSuccess(instance *craftypathv1alpha1.SopsSecret, result controllerutil.OperationResult) (reconcile.Result, error) {
	status := craftypathv1alpha1.SopsSecretStatus{
		LastUpdate: metav1.Now(),
		Reason:     "",
		Status:     "Success",
	}
	instance.Status = status

	if err := r.client.Status().Update(context.Background(), instance); err != nil {
		log.Error(err, "unable to update status")
		r.recorder.Event(instance, "Warning", "ProcessingError", "Unable to update status")
		return reconcile.Result{
			RequeueAfter: time.Second,
			Requeue:      true,
		}, nil
	}

	opResult := capitalizeFirst(string(result))
	r.recorder.Event(instance, "Normal", opResult, fmt.Sprintf("%s secret: %s", opResult, instance.Name))
	return reconcile.Result{}, nil
}

func capitalizeFirst(s string) string {
	if len(s) == 0 {
		return ""
	}
	tmp := []rune(s)
	tmp[0] = unicode.ToUpper(tmp[0])
	return string(tmp)
}
