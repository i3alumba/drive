package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"testing"
)

func TestComputerUploadPersistsBytesAndCleansMultipartTemporaryFile(t *testing.T) {
	store := testTransferStore()
	s := New(store, nil, nil)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("path", "docs")
	file, err := form.CreateFormFile("file", "upload.txt")
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 9<<20)
	_, _ = file.Write(payload)
	_ = form.Close()
	r := ownerRequest("POST", "/api/upload", "", "42")
	r.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
	r.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	s.uploadFile(w, r)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	store.mu.Lock()
	if !bytes.Equal(store.data["users/42/docs/upload.txt"], payload) {
		t.Error("server acknowledged incorrect upload bytes")
	}
	store.mu.Unlock()
	reopened, err := r.MultipartForm.File["file"][0].Open()
	if err == nil {
		reopened.Close()
		t.Fatal("multipart temporary file was not removed")
	}
	if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
