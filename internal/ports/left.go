package ports

type ServerPort interface {
	Serve(host string, port int) error
}
