package proto

import "google.golang.org/protobuf/proto"

func MarshalAnnounceRequest(m *AnnounceRequest) ([]byte, error) {
	return proto.Marshal(m)
}

func UnmarshalAnnounceRequest(b []byte) (*AnnounceRequest, error) {
	m := &AnnounceRequest{}
	return m, proto.Unmarshal(b, m)
}

func MarshalAnnounceResponse(m *AnnounceResponse) ([]byte, error) {
	return proto.Marshal(m)
}

func UnmarshalAnnounceResponse(b []byte) (*AnnounceResponse, error) {
	m := &AnnounceResponse{}
	return m, proto.Unmarshal(b, m)
}
