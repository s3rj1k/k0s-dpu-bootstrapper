// SPDX-FileCopyrightText: 2026 k0s-dpu-bootstrapper authors
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"

	"code.local/k0s-dpu-bootstrapper/internal/controller"
)

// RecordedEvent is one Event and the name of the object it was recorded on.
type RecordedEvent struct {
	Object  string
	Reason  string
	Message string
}

// Recorder captures Events with the object each one names. The fake recorder in client-go
// formats only the message, so a test using it cannot tell which object was reported on.
type Recorder struct {
	Events []RecordedEvent
}

func (r *Recorder) record(object runtime.Object, reason, message string) {
	name := fmt.Sprintf("%T", object)
	if accessor, err := meta.Accessor(object); err == nil {
		name = accessor.GetName()
	}

	r.Events = append(r.Events, RecordedEvent{Object: name, Reason: reason, Message: message})
}

// Event records what happened to one object.
func (r *Recorder) Event(object runtime.Object, _, reason, message string) {
	r.record(object, reason, message)
}

// Eventf records a formatted message.
func (r *Recorder) Eventf(object runtime.Object, _, reason, messageFmt string, args ...any) {
	r.record(object, reason, fmt.Sprintf(messageFmt, args...))
}

// AnnotatedEventf records a formatted message, dropping the annotations.
func (r *Recorder) AnnotatedEventf(object runtime.Object, _ map[string]string, _, reason, messageFmt string, args ...any) {
	r.record(object, reason, fmt.Sprintf(messageFmt, args...))
}

// Recorded returns the capturing recorder a reconciler under test was built with.
func Recorded(t *testing.T, r *controller.DPUReconciler) *Recorder {
	t.Helper()

	recorder, ok := r.Recorder.(*Recorder)
	if !ok {
		t.Fatalf("recorder is a %T, not the capturing one", r.Recorder)
	}

	return recorder
}

// ExpectEvents checks what was reported under one reason, and on which objects. Naming the
// objects is the point, since reporting twice on the DPU has to fail as loudly as silence.
func ExpectEvents(t *testing.T, r *controller.DPUReconciler, reason string, objects ...string) {
	t.Helper()

	got := []string{}

	for _, event := range Recorded(t, r).Events {
		if event.Reason == reason {
			got = append(got, event.Object)
		}
	}

	if len(got) != len(objects) {
		t.Errorf("events reported as %s = %v, want one on each of %v", reason, got, objects)

		return
	}

	for _, want := range objects {
		if !slices.Contains(got, want) {
			t.Errorf("nothing was reported as %s on %s, only on %v", reason, want, got)
		}
	}
}

// ExpectEventMessage checks that one reason was reported with a message naming a substring.
func ExpectEventMessage(t *testing.T, r *controller.DPUReconciler, reason, want string) {
	t.Helper()

	for _, event := range Recorded(t, r).Events {
		if event.Reason == reason && strings.Contains(event.Message, want) {
			return
		}
	}

	t.Errorf("no %s event carries %q, recorded %+v", reason, want, Recorded(t, r).Events)
}
