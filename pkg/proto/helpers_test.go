package proto_test

import (
	"testing"

	proto "github.com/davidbirdsong/shoal/pkg/proto"
)

func TestAnnounceRequestRoundTrip(t *testing.T) {
	orig := &proto.AnnounceRequest{Addr: "10.0.1.5", Port: 43821, State: "ready"}
	b, err := proto.MarshalAnnounceRequest(orig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := proto.UnmarshalAnnounceRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Addr != orig.Addr || got.Port != orig.Port || got.State != orig.State {
		t.Errorf("got %+v, want %+v", got, orig)
	}
}

func TestAnnounceResponseRoundTrip(t *testing.T) {
	orig := &proto.AnnounceResponse{Accepted: true, BackendKey: "pool/task-1", Reason: ""}
	b, err := proto.MarshalAnnounceResponse(orig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := proto.UnmarshalAnnounceResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Accepted != orig.Accepted || got.BackendKey != orig.BackendKey {
		t.Errorf("got %+v, want %+v", got, orig)
	}
}

func TestAnnounceResponseRejected(t *testing.T) {
	orig := &proto.AnnounceResponse{Accepted: false, Reason: "not ready"}
	b, err := proto.MarshalAnnounceResponse(orig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := proto.UnmarshalAnnounceResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Accepted || got.Reason != orig.Reason {
		t.Errorf("got %+v, want %+v", got, orig)
	}
}

func TestDepartRequestRoundTrip(t *testing.T) {
	orig := &proto.DepartRequest{Addr: "10.0.1.5", Port: 43821, TimeoutSeconds: 30}
	b, err := proto.MarshalDepartRequest(orig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := proto.UnmarshalDepartRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Addr != orig.Addr || got.Port != orig.Port || got.TimeoutSeconds != orig.TimeoutSeconds {
		t.Errorf("got %+v, want %+v", got, orig)
	}
}

func TestDepartResponseRoundTrip(t *testing.T) {
	orig := &proto.DepartResponse{Accepted: true}
	b, err := proto.MarshalDepartResponse(orig)
	if err != nil {
		t.Fatal(err)
	}
	got, err := proto.UnmarshalDepartResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Accepted != orig.Accepted {
		t.Errorf("got %+v, want %+v", got, orig)
	}
}

func TestUnmarshalGarbage(t *testing.T) {
	garbage := []byte{0xff, 0xfe, 0x00, 0x01}
	if _, err := proto.UnmarshalAnnounceRequest(garbage); err == nil {
		t.Error("expected error unmarshaling garbage bytes")
	}
	if _, err := proto.UnmarshalDepartRequest(garbage); err == nil {
		t.Error("expected error unmarshaling garbage bytes")
	}
}

func TestUnmarshalEmpty(t *testing.T) {
	// Empty bytes should produce a zero-value message, not an error.
	got, err := proto.UnmarshalAnnounceRequest([]byte{})
	if err != nil {
		t.Fatalf("unexpected error on empty bytes: %v", err)
	}
	if got.Addr != "" || got.Port != 0 {
		t.Errorf("expected zero-value message, got %+v", got)
	}
}
