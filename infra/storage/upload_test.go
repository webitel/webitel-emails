package storage

import (
	"context"
	"strings"
	"testing"

	storagegrpc "buf.build/gen/go/webitel/storage/grpc/go/_gogrpc"
	storagepb "buf.build/gen/go/webitel/storage/protocolbuffers/go"
	"google.golang.org/grpc"
)

type uploadStream struct {
	grpc.ClientStream
	sent []*storagepb.UploadFileRequest
}

func (s *uploadStream) Send(m *storagepb.UploadFileRequest) error {
	s.sent = append(s.sent, m)
	return nil
}

func (s *uploadStream) CloseAndRecv() (*storagepb.UploadFileResponse, error) {
	return &storagepb.UploadFileResponse{FileId: 1}, nil
}

type uploadAPI struct {
	storagegrpc.FileServiceClient
	stream *uploadStream
}

func (a uploadAPI) UploadFile(context.Context, ...grpc.CallOption) (grpc.ClientStreamingClient[storagepb.UploadFileRequest, storagepb.UploadFileResponse], error) {
	return a.stream, nil
}

func TestSendUploadCarriesUploader(t *testing.T) {
	stream := &uploadStream{}
	_, err := sendUpload(context.Background(), uploadAPI{stream: stream}, UploadRequest{
		DomainID: 7, Name: "a/b.pdf", MimeType: "application/pdf", ReferenceID: "ref", UploadedBy: 70, Content: []byte("x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	meta := stream.sent[0].GetMetadata()
	if meta.GetUploadedBy() != 70 || meta.GetUuid() != "ref" || meta.GetName() != "a_b.pdf" ||
		meta.GetChannel() != storagepb.UploadFileChannel_MailChannel {
		t.Fatalf("metadata = %+v", meta)
	}
}

// The service stores the name it cleaned and storage cleans it again, so a second
// call must not change it.
func TestSanitizeFileNameIsStable(t *testing.T) {
	long := strings.Repeat("a", 199)
	for _, name := range []string{long + " b", long + ".b", ". a .", " .a. ", "a\tb/c", "", " . "} {
		once := SanitizeFileName(name)
		if again := SanitizeFileName(once); again != once {
			t.Errorf("SanitizeFileName(%q) = %q, then %q", name, once, again)
		}
	}
	if got := SanitizeFileName(long + " b"); got != long {
		t.Errorf("cut name = %q, want the 199 letters without the trailing space", got)
	}
}
