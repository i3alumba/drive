package main

import (
	"encoding/json"
	"net/http"
	"os"

	log "github.com/sirupsen/logrus"
)

type Credentials struct {
	Passphrase string
}

func validate(w http.ResponseWriter, r *http.Request) {
	connectionIP := r.Header.Get("X-Real-Ip")
	log.Info("/validate/", connectionIP)

	var c Credentials
	err := json.NewDecoder(r.Body).Decode(&c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Error(err.Error())
		return
	}

	p := os.Getenv("PASSPHRASE")
	if p == "" {
		http.Error(w, "Could not acquire correct passphrase", http.StatusServiceUnavailable)
		log.Error("Could not acquire passphrase")
		return
	}

	if p != c.Passphrase {
		http.Error(w, "Invalid passphrase", http.StatusUnprocessableEntity)
		log.Error("Invalid passphrase")
		return
	}
}

func main() {
	debug := os.Getenv("DEBUG") != ""

	log.SetFormatter(&log.TextFormatter{})
	log.SetOutput(os.Stdout)

	if debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}

	log.Debug("Creating ServeMux...")

	mux := http.NewServeMux()

	log.Debug("ServeMux created successfully")

	mux.HandleFunc("POST /validate/", validate)

	log.Info("Starting auth server...")

	err := http.ListenAndServe("0.0.0.0:8000", mux)
	if err != nil {
		log.Fatal("Unable to start server")
	}
}
