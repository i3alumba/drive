package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"

	"api/internal/ports"
)

type HTTPServerAdapter struct {
	storage ports.StoragePort
	auth    ports.AuthPort
}

func NewHTTPServeAdapter(storage ports.StoragePort, auth ports.AuthPort) *HTTPServerAdapter {
	return &HTTPServerAdapter{storage: storage, auth: auth}
}

func (a *HTTPServerAdapter) addMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, err := a.auth.GetUsername(r.Header)
		if err != nil {
			http.Error(w, fmt.Sprintf("Unable to authorize: %v\n", err), http.StatusForbidden)
		}
		ctx := context.WithValue(r.Context(), "username", username)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *HTTPServerAdapter) info(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	username := ctx.Value("username").(string)
	pathstring := r.URL.Path
	pathstring, _ = strings.CutPrefix(pathstring, "/info")

	if !strings.HasSuffix(pathstring, "/") {
		objectInfo, err := a.storage.StatObject(username, pathstring)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to stat object %v: %v\n", pathstring, err), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(objectInfo)
	} else {
		objects, err := a.storage.ListObjects(username, pathstring)
		if err != nil {
			http.Error(w, fmt.Sprintf("Unable to list directory: %v\n", err), http.StatusNotFound)
			return
		}

		var objectsArr []ports.ObjectInfo
		for obj := range objects {
			objectsArr = append(objectsArr, obj)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(objectsArr)
	}
}

func (a *HTTPServerAdapter) download(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	username := ctx.Value("username").(string)
	pathstring := r.URL.Path
	pathstring, _ = strings.CutPrefix(pathstring, "/download")

	object, err := a.storage.GetObject(username, pathstring)
	if err != nil {
		http.Error(w, fmt.Sprintf("Unable to get object %v: %v\n", pathstring, err), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "octet-stream")
	_, fname := path.Split(pathstring)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%v\"", fname))

	var object_bytes []byte
	_, err = object.Read(object_bytes)
	if err != nil {
		http.Error(w, "Unable to read file contents", http.StatusInternalServerError)
		return
	}
	_, err = w.Write(object_bytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *HTTPServerAdapter) upload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	username := ctx.Value("username").(string)
	pathstring := r.URL.Path
	pathstring, _ = strings.CutPrefix(pathstring, "/upload")
}

func (a HTTPServerAdapter) Serve(host string, port int) error {
	mux := http.NewServeMux()
	mux.Handle("GET /info/", a.addMiddleware(http.HandlerFunc(a.info)))
	mux.Handle("GET /download/", a.addMiddleware(http.HandlerFunc(a.download)))

	err := http.ListenAndServe(host+":"+strconv.Itoa(port), mux)
	return err
}
