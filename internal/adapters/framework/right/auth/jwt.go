package auth

import (
	"strings"

	"api/internal/ports"
)

type JWTAuthAdapter struct {
	endpoint string
}

func NewJWTAdapter(endpoint string) *JWTAuthAdapter {
	return &JWTAuthAdapter{endpoint: endpoint}
}

func (a *JWTAuthAdapter) GetUsername(credentials ports.Metadata) (string, error) {
	token := credentials.Get("Authorization")
	token, _ = strings.CutPrefix(token, "Bearer: ")

	return "i3alumba", nil // consider rewriting
}
