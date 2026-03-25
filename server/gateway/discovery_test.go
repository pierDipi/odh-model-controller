package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// --- isInfrastructureError tests ---

func TestIsInfrastructureError_NetworkError(t *testing.T) {
	netErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	if !isInfrastructureError(netErr) {
		t.Error("expected net.OpError to be an infrastructure error")
	}
}

func TestIsInfrastructureError_WrappedNetworkError(t *testing.T) {
	netErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	wrapped := fmt.Errorf("self subject access review: %w", netErr)
	if !isInfrastructureError(wrapped) {
		t.Error("expected wrapped net.OpError to be an infrastructure error")
	}
}

func TestIsInfrastructureError_K8s5xxError(t *testing.T) {
	statusErr := &apierrors.StatusError{
		ErrStatus: metav1.Status{
			Code:   502,
			Reason: metav1.StatusReasonInternalError,
		},
	}
	if !isInfrastructureError(statusErr) {
		t.Error("expected 502 StatusError to be an infrastructure error")
	}
}

func TestIsInfrastructureError_Wrapped5xxError(t *testing.T) {
	statusErr := &apierrors.StatusError{
		ErrStatus: metav1.Status{
			Code:   500,
			Reason: metav1.StatusReasonInternalError,
		},
	}
	wrapped := fmt.Errorf("self subject access review: %w", statusErr)
	if !isInfrastructureError(wrapped) {
		t.Error("expected wrapped 500 StatusError to be an infrastructure error")
	}
}

func TestIsInfrastructureError_K8s403Error(t *testing.T) {
	statusErr := apierrors.NewForbidden(schema.GroupResource{Group: "authorization.k8s.io", Resource: "selfsubjectaccessreviews"}, "", errors.New("forbidden"))
	if isInfrastructureError(statusErr) {
		t.Error("expected 403 StatusError NOT to be an infrastructure error")
	}
}

func TestIsInfrastructureError_K8s401Error(t *testing.T) {
	statusErr := apierrors.NewUnauthorized("unauthorized")
	if isInfrastructureError(statusErr) {
		t.Error("expected 401 StatusError NOT to be an infrastructure error")
	}
}

func TestIsInfrastructureError_GenericError(t *testing.T) {
	if isInfrastructureError(errors.New("something went wrong")) {
		t.Error("expected generic error NOT to be an infrastructure error")
	}
}

func TestIsInfrastructureError_EvaluationError(t *testing.T) {
	err := fmt.Errorf("access review evaluation error: %s", "some evaluation problem")
	if isInfrastructureError(err) {
		t.Error("expected evaluation error NOT to be an infrastructure error")
	}
}

// --- Discover error-classification tests ---

type mockAccessChecker struct {
	allowed bool
	err     error
}

func (m *mockAccessChecker) CheckAccess(_ context.Context, _, _ string) (bool, error) {
	return m.allowed, m.err
}

func TestDiscover_NetworkErrorReturnsError(t *testing.T) {
	netErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	d := &KubeDiscoverer{
		AccessChecker: &mockAccessChecker{err: fmt.Errorf("self subject access review: %w", netErr)},
	}

	_, err := d.Discover(context.Background(), "token", "ns")
	if err == nil {
		t.Fatal("expected error for network failure, got nil")
	}
}

func TestDiscover_5xxErrorReturnsError(t *testing.T) {
	statusErr := &apierrors.StatusError{
		ErrStatus: metav1.Status{
			Code:   502,
			Reason: metav1.StatusReasonInternalError,
		},
	}
	d := &KubeDiscoverer{
		AccessChecker: &mockAccessChecker{err: fmt.Errorf("self subject access review: %w", statusErr)},
	}

	_, err := d.Discover(context.Background(), "token", "ns")
	if err == nil {
		t.Fatal("expected error for 5xx failure, got nil")
	}
}

func TestDiscover_403ReturnsEmptyList(t *testing.T) {
	statusErr := apierrors.NewForbidden(schema.GroupResource{Group: "authorization.k8s.io", Resource: "selfsubjectaccessreviews"}, "", errors.New("forbidden"))
	d := &KubeDiscoverer{
		AccessChecker: &mockAccessChecker{err: statusErr},
	}

	refs, err := d.Discover(context.Background(), "token", "ns")
	if err != nil {
		t.Fatalf("expected nil error for 403, got: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected empty list, got %d refs", len(refs))
	}
}

func TestDiscover_401ReturnsEmptyList(t *testing.T) {
	statusErr := apierrors.NewUnauthorized("unauthorized")
	d := &KubeDiscoverer{
		AccessChecker: &mockAccessChecker{err: statusErr},
	}

	refs, err := d.Discover(context.Background(), "token", "ns")
	if err != nil {
		t.Fatalf("expected nil error for 401, got: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected empty list, got %d refs", len(refs))
	}
}

func TestDiscover_GenericErrorReturnsEmptyList(t *testing.T) {
	d := &KubeDiscoverer{
		AccessChecker: &mockAccessChecker{err: errors.New("create user client: config error")},
	}

	refs, err := d.Discover(context.Background(), "token", "ns")
	if err != nil {
		t.Fatalf("expected nil error for generic error, got: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected empty list, got %d refs", len(refs))
	}
}

func TestDiscover_NotAllowedReturnsEmptyList(t *testing.T) {
	d := &KubeDiscoverer{
		AccessChecker: &mockAccessChecker{allowed: false},
	}

	refs, err := d.Discover(context.Background(), "token", "ns")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected empty list, got %d refs", len(refs))
	}
}
