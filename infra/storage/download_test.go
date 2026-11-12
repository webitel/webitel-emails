package storage

import (
	"context"
	"errors"
	"io"
	"testing"

	storagepb "buf.build/gen/go/webitel/storage/protocolbuffers/go"
	"google.golang.org/grpc"
)

type chunkStream struct {
	grpc.ClientStream
	chunks [][]byte
}

func (s *chunkStream) Recv() (*storagepb.StreamFile, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	chunk := s.chunks[0]
	s.chunks = s.chunks[1:]
	return &storagepb.StreamFile{Data: &storagepb.StreamFile_Chunk{Chunk: chunk}}, nil
}

// storage ends a stream cleanly after a read error, so only the size tells.
func TestFileReaderChecksSize(t *testing.T) {
	for _, tt := range []struct {
		name string
		size int64
		want error
	}{
		{"exact", 6, nil},
		{"short with a clean end", 7, ErrFileIncomplete},
		{"longer", 5, ErrFileIncomplete},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &fileReader{ctx: context.Background(), left: tt.size, close: func() {},
				stream: &chunkStream{chunks: [][]byte{[]byte("abc"), []byte("def")}}}
			_, err := io.ReadAll(r)
			if !errors.Is(err, tt.want) && !(tt.want == nil && err == nil) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}
