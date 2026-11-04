package service

import (
	"strings"
	"testing"
	"unicode/utf8"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
)

func TestParseThreadSort(t *testing.T) {
	cases := map[string][]model.EmailThreadSort{
		"":                   {{Field: model.EmailThreadSortLastMessageAt, Desc: true}},
		" , ":                {{Field: model.EmailThreadSortLastMessageAt, Desc: true}},
		"created_at":         {{Field: model.EmailThreadSortCreatedAt}},
		"+updated_at":        {{Field: model.EmailThreadSortUpdatedAt}},
		"-status_changed_at": {{Field: model.EmailThreadSortStatusChangedAt, Desc: true}},
		"-last_message_at, created_at": {
			{Field: model.EmailThreadSortLastMessageAt, Desc: true}, {Field: model.EmailThreadSortCreatedAt},
		},
	}
	for value, want := range cases {
		got, err := ParseThreadSort(value)
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%q: %+v", value, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%q: %+v, want %+v", value, got, want)
			}
		}
	}

	for _, value := range []string{"id", "subject", "-", "created_at,created_at", "last_message_at;drop"} {
		if _, err := ParseThreadSort(value); kiterrors.ID(err) != "email.thread.invalid_sort" {
			t.Errorf("%q: error %v", value, err)
		}
	}
}

func TestThreadFilterValidation(t *testing.T) {
	filter, err := threadFilter(1, 70, EmailThreadListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if filter.Page != 1 || filter.Size != defaultThreadPageSize || filter.Assignment != model.EmailThreadAssignmentAll ||
		filter.DomainID != 1 || filter.UserID != 70 || len(filter.Sort) != 1 {
		t.Fatalf("defaults %+v", filter)
	}
	if filter, err = threadFilter(1, 70, EmailThreadListRequest{Size: maxThreadPageSize}); err != nil || filter.Size != 100 {
		t.Fatalf("maximum size: %+v, %v", filter, err)
	}

	invalid := []EmailThreadListRequest{
		{Page: -1},
		{Size: -1},
		{Size: maxThreadPageSize + 1},
		{Assignment: "everyone"},
		{Statuses: []model.EmailThreadStatus{"deleted"}},
		{ProfileIDs: []int64{0}},
		{ContactID: -1},
	}
	for _, req := range invalid {
		if _, err := threadFilter(1, 70, req); kiterrors.ID(err) != "email.thread.invalid_filter" {
			t.Errorf("%+v: error %v", req, err)
		}
	}
}

func TestNormalizePreview(t *testing.T) {
	view := &model.EmailThreadView{Preview: &model.EmailThreadPreview{Text: "  Hello,\n\n\tworld  \r\n again "}}
	normalizePreview(view)
	if view.Preview.Text != "Hello, world again" {
		t.Fatalf("whitespace: %q", view.Preview.Text)
	}

	long := &model.EmailThreadView{Preview: &model.EmailThreadPreview{Text: strings.Repeat("ї", 300)}}
	normalizePreview(long)
	if utf8.RuneCountInString(long.Preview.Text) != threadPreviewRunes || !utf8.ValidString(long.Preview.Text) {
		t.Fatalf("cut: %d runes", utf8.RuneCountInString(long.Preview.Text))
	}

	empty := &model.EmailThreadView{Preview: &model.EmailThreadPreview{}}
	normalizePreview(empty)
	if empty.Preview.Text != "" {
		t.Fatalf("an HTML-only email got a preview %q", empty.Preview.Text)
	}
	normalizePreview(&model.EmailThreadView{})
}
