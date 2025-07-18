package promotion

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	admissionv1 "k8s.io/api/admission/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kargoapi "github.com/akuity/kargo/api/v1alpha1"
	"github.com/akuity/kargo/internal/api"
	kargoEvent "github.com/akuity/kargo/internal/event"
	"github.com/akuity/kargo/internal/kargo"
	libEvent "github.com/akuity/kargo/internal/kubernetes/event"
	"github.com/akuity/kargo/internal/logging"
	libWebhook "github.com/akuity/kargo/internal/webhook/kubernetes"
)

var (
	promotionGroupKind = schema.GroupKind{
		Group: kargoapi.GroupVersion.Group,
		Kind:  "Promotion",
	}
	promotionGroupResource = schema.GroupResource{
		Group:    kargoapi.GroupVersion.Group,
		Resource: "Promotion",
	}
)

type webhook struct {
	client  client.Client
	decoder admission.Decoder

	recorder record.EventRecorder

	// The following behaviors are overridable for testing purposes:

	getFreightFn func(
		context.Context,
		client.Client,
		types.NamespacedName,
	) (*kargoapi.Freight, error)

	getStageFn func(
		context.Context,
		client.Client,
		types.NamespacedName,
	) (*kargoapi.Stage, error)

	validateProjectFn func(
		context.Context,
		client.Client,
		client.Object,
	) error

	authorizeFn func(
		ctx context.Context,
		promo *kargoapi.Promotion,
		action string,
	) error

	admissionRequestFromContextFn func(context.Context) (admission.Request, error)

	createSubjectAccessReviewFn func(
		context.Context,
		client.Object,
		...client.CreateOption,
	) error

	isRequestFromKargoControlplaneFn libWebhook.IsRequestFromKargoControlplaneFn
}

func SetupWebhookWithManager(
	ctx context.Context,
	cfg libWebhook.Config,
	mgr ctrl.Manager,
) error {
	w := newWebhook(
		cfg,
		mgr.GetClient(),
		admission.NewDecoder(mgr.GetScheme()),
		libEvent.NewRecorder(ctx, mgr.GetScheme(), mgr.GetClient(), "promotion-webhook"),
	)
	return ctrl.NewWebhookManagedBy(mgr).
		For(&kargoapi.Promotion{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

func newWebhook(
	cfg libWebhook.Config,
	kubeClient client.Client,
	decoder admission.Decoder,
	recorder record.EventRecorder,
) *webhook {
	w := &webhook{
		client:   kubeClient,
		decoder:  decoder,
		recorder: recorder,
	}
	w.getFreightFn = api.GetFreight
	w.getStageFn = api.GetStage
	w.validateProjectFn = libWebhook.ValidateProject
	w.authorizeFn = w.authorize
	w.admissionRequestFromContextFn = admission.RequestFromContext
	w.createSubjectAccessReviewFn = w.client.Create
	w.isRequestFromKargoControlplaneFn = libWebhook.IsRequestFromKargoControlplane(cfg.ControlplaneUserRegex)
	return w
}

func (w *webhook) Default(ctx context.Context, obj runtime.Object) error {
	req, err := w.admissionRequestFromContextFn(ctx)
	if err != nil {
		return fmt.Errorf("get admission request from context: %w", err)
	}

	promo := obj.(*kargoapi.Promotion) // nolint: forcetypeassert
	var oldPromo *kargoapi.Promotion
	// We need to decode old object manually since controller-runtime doesn't decode it for us.
	if req.Operation == admissionv1.Update {
		oldPromo = &kargoapi.Promotion{}
		if err = w.decoder.DecodeRaw(req.OldObject, oldPromo); err != nil {
			return fmt.Errorf("decode old object: %w", err)
		}
	}

	if promo.Annotations == nil {
		promo.Annotations = make(map[string]string, 1)
	}

	switch req.Operation {
	case admissionv1.Create:
		// Set actor as an admission request's user info when the promotion is created
		// to allow controllers to track who created it.
		if !w.isRequestFromKargoControlplaneFn(req) {
			promo.Annotations[kargoapi.AnnotationKeyCreateActor] = api.FormatEventKubernetesUserActor(req.UserInfo)
		}

		// Enrich the annotation with the actor and control plane information.
		w.setAbortAnnotationActor(req, nil, promo)

		// Inflate any PromotionTasks in the Promotion's steps
		if err = kargo.NewPromotionBuilder(w.client).InflateSteps(ctx, promo); err != nil {
			return fmt.Errorf("failed to inflate Promotion steps: %w", err)
		}
	case admissionv1.Update:
		// Ensure actor annotation immutability
		if oldActor, ok := oldPromo.Annotations[kargoapi.AnnotationKeyCreateActor]; ok {
			promo.Annotations[kargoapi.AnnotationKeyCreateActor] = oldActor
		} else {
			delete(promo.Annotations, kargoapi.AnnotationKeyCreateActor)
		}

		// Enrich the annotation with the actor and control plane information.
		w.setAbortAnnotationActor(req, oldPromo, promo)
	}

	stage, err := w.getStageFn(
		ctx,
		w.client,
		types.NamespacedName{
			Namespace: promo.Namespace,
			Name:      promo.Spec.Stage,
		},
	)
	if err != nil {
		return fmt.Errorf(
			"error finding Stage %q in namespace %q: %w",
			promo.Spec.Stage,
			promo.Namespace,
			err,
		)
	}
	if stage == nil {
		return fmt.Errorf(
			"could not find Stage %q in namespace %q",
			promo.Spec.Stage,
			promo.Namespace,
		)
	}
	if len(promo.Spec.Steps) == 0 {
		// nolint:staticcheck
		return fmt.Errorf(
			"Stage %q in namespace %q defines no promotion steps",
			promo.Spec.Stage,
			promo.Namespace,
		)
	}

	// Make sure the Promotion has the same shard as the Stage
	if stage.Spec.Shard != "" {
		if promo.Labels == nil {
			promo.Labels = make(map[string]string, 1)
		}
		promo.Labels[kargoapi.LabelKeyShard] = stage.Spec.Shard
	} else {
		delete(promo.Labels, kargoapi.LabelKeyShard)
	}

	ownerRef := metav1.NewControllerRef(stage, kargoapi.GroupVersion.WithKind("Stage"))
	promo.OwnerReferences = []metav1.OwnerReference{*ownerRef}
	return nil
}

func (w *webhook) ValidateCreate(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	promo := obj.(*kargoapi.Promotion) // nolint: forcetypeassert

	if err := w.validateProjectFn(ctx, w.client, promo); err != nil {
		var statusErr *apierrors.StatusError
		if ok := errors.As(err, &statusErr); ok {
			return nil, statusErr
		}
		var fieldErr *field.Error
		if ok := errors.As(err, &fieldErr); !ok {
			return nil, apierrors.NewInternalError(err)
		}
		return nil, apierrors.NewInvalid(
			promotionGroupKind,
			promo.Name,
			field.ErrorList{fieldErr},
		)
	}

	if err := w.authorizeFn(ctx, promo, "create"); err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	req, err := w.admissionRequestFromContextFn(ctx)
	if err != nil {
		return nil, apierrors.NewInternalError(
			fmt.Errorf("get admission request from context: %w", err),
		)
	}

	stage, err := w.getStageFn(ctx, w.client, types.NamespacedName{
		Namespace: promo.Namespace,
		Name:      promo.Spec.Stage,
	})
	if err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("get stage: %w", err))
	}

	freight, err := w.getFreightFn(ctx, w.client, types.NamespacedName{
		Namespace: promo.Namespace,
		Name:      promo.Spec.Freight,
	})
	if err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("get freight: %w", err))
	}

	if !stage.IsFreightAvailable(freight) {
		return nil, apierrors.NewInvalid(
			promotionGroupKind,
			promo.Name,
			field.ErrorList{
				field.Invalid(
					field.NewPath("spec", "freight"),
					promo.Spec.Freight,
					"Freight is not available to this Stage",
				),
			},
		)
	}

	// Record Promotion created event if the request doesn't come from Kargo controlplane
	if !w.isRequestFromKargoControlplaneFn(req) {
		w.recordPromotionCreatedEvent(ctx, req, promo, freight)
	}

	return nil, nil
}

func (w *webhook) ValidateUpdate(
	ctx context.Context,
	oldObj runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	promo := newObj.(*kargoapi.Promotion) // nolint: forcetypeassert
	if err := w.authorizeFn(ctx, promo, "update"); err != nil {
		return nil, apierrors.NewInternalError(err)
	}

	// PromotionSpecs are meant to be immutable
	// nolint: forcetypeassert
	if !reflect.DeepEqual(promo.Spec, oldObj.(*kargoapi.Promotion).Spec) {
		return nil, apierrors.NewInvalid(
			promotionGroupKind,
			promo.Name,
			field.ErrorList{
				field.Invalid(
					field.NewPath("spec"),
					promo.Spec,
					"spec is immutable",
				),
			},
		)
	}

	return nil, nil
}

func (w *webhook) ValidateDelete(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	promo := obj.(*kargoapi.Promotion) // nolint: forcetypeassert
	return nil, w.authorizeFn(ctx, promo, "delete")
}

func (w *webhook) authorize(
	ctx context.Context,
	promo *kargoapi.Promotion,
	action string,
) error {
	logger := logging.LoggerFromContext(ctx)

	req, err := w.admissionRequestFromContextFn(ctx)
	if err != nil {
		logger.Error(err, "")
		return apierrors.NewForbidden(
			promotionGroupResource,
			promo.Name,
			fmt.Errorf(
				"error retrieving admission request from context; refusing to "+
					"%s Promotion",
				action,
			),
		)
	}

	accessReview := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   req.UserInfo.Username,
			Groups: req.UserInfo.Groups,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Group:     kargoapi.GroupVersion.Group,
				Resource:  "stages",
				Name:      promo.Spec.Stage,
				Verb:      "promote",
				Namespace: promo.Namespace,
			},
		},
	}
	if err := w.createSubjectAccessReviewFn(ctx, accessReview); err != nil {
		logger.Error(err, "")
		return apierrors.NewForbidden(
			promotionGroupResource,
			promo.Name,
			fmt.Errorf(
				"error creating SubjectAccessReview; refusing to %s Promotion",
				action,
			),
		)
	}

	if !accessReview.Status.Allowed {
		return apierrors.NewForbidden(
			promotionGroupResource,
			promo.Name,
			fmt.Errorf(
				"subject %q is not permitted to %s Promotions for Stage %q",
				req.UserInfo.Username,
				action,
				promo.Spec.Stage,
			),
		)
	}

	return nil
}

func (w *webhook) recordPromotionCreatedEvent(
	ctx context.Context,
	req admission.Request,
	p *kargoapi.Promotion,
	f *kargoapi.Freight,
) {
	actor := api.FormatEventKubernetesUserActor(req.UserInfo)
	w.recorder.AnnotatedEventf(
		p,
		kargoEvent.NewPromotionAnnotations(ctx, actor, p, f),
		corev1.EventTypeNormal,
		kargoapi.EventReasonPromotionCreated,
		"Promotion created for Stage %q by %q",
		p.Spec.Stage,
		actor,
	)
}

func (w *webhook) setAbortAnnotationActor(req admission.Request, existing, updated *kargoapi.Promotion) {
	if abortReq, ok := api.AbortPromotionAnnotationValue(updated.Annotations); ok {
		var oldAbortReq *kargoapi.AbortPromotionRequest
		if existing != nil {
			oldAbortReq, _ = api.AbortPromotionAnnotationValue(existing.Annotations)
		}
		// If the abort request has changed, enrich the annotation with the
		// actor and control plane information.
		if existing == nil || oldAbortReq == nil || !abortReq.Equals(oldAbortReq) {
			abortReq.ControlPlane = w.isRequestFromKargoControlplaneFn(req)
			if !abortReq.ControlPlane {
				// If the abort request is not from the control plane, then it's
				// from a specific Kubernetes user. Without this check we would
				// overwrite the actor field set by the control plane.
				abortReq.Actor = api.FormatEventKubernetesUserActor(req.UserInfo)
			}
			updated.Annotations[kargoapi.AnnotationKeyAbort] = abortReq.String()
		}
	}
}
