package storage

import (
	"testing"
	"time"

	storagepb "buf.build/gen/go/webitel/storage/protocolbuffers/go"
)

func TestDownloadLinkRequest(t *testing.T) {
	req := DownloadLinkRequest(3, 77, 5*time.Minute)
	if req.GetDomainId() != 3 || req.GetFileId() != 77 || req.GetAction() != "download" || req.GetSource() != "" ||
		req.GetMetadata() || len(req.GetQuery()) != 1 || req.GetQuery()["expires"] != "300000" {
		t.Fatalf("request %+v", req)
	}
}

func TestResolveLink(t *testing.T) {
	signed := "/any/file/77/download?domain_id=3&expires=1790000000000&signature=ab+c/d=="
	for base, want := range map[string]string{
		"https://storage.example":           "https://storage.example/any/file/77/download?domain_id=3&expires=1790000000000&signature=ab+c/d==",
		"https://storage.example/":          "https://storage.example/any/file/77/download?domain_id=3&expires=1790000000000&signature=ab+c/d==",
		"https://host.example/api/storage/": "https://host.example/api/storage/any/file/77/download?domain_id=3&expires=1790000000000&signature=ab+c/d==",
		"http://10.0.0.1:10021/prefix":      "http://10.0.0.1:10021/prefix/any/file/77/download?domain_id=3&expires=1790000000000&signature=ab+c/d==",
	} {
		got, err := ResolveLink(&storagepb.GenerateFileLinkResponse{BaseUrl: base, Url: signed})
		if err != nil || got != want {
			t.Errorf("%s: %q, %v", base, got, err)
		}
	}

	for _, bad := range []*storagepb.GenerateFileLinkResponse{
		{BaseUrl: "", Url: signed},
		{BaseUrl: "/relative", Url: signed},
		{BaseUrl: "storage.example", Url: signed},
		{BaseUrl: "https://storage.example", Url: ""},
	} {
		if got, err := ResolveLink(bad); err == nil {
			t.Errorf("%+v resolved to %q", bad, got)
		}
	}
}
