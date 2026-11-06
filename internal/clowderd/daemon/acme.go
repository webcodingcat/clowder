package daemon

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"

	"github.com/webcodingcat/clowder/internal/clowderd/daemon/loop"
	"github.com/webcodingcat/clowder/internal/ingress"
	"github.com/webcodingcat/clowder/internal/state"
)

// challengeHandler answers ACME's HTTP-01 challenges. The leader puts them
// in the state while it orders a certificate. Every agent serves them, so
// whichever host holds the Floating IP can answer the CA.
func challengeHandler(cluster loop.Cluster) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+ingress.ChallengePath+"{token}", func(w http.ResponseWriter, r *http.Request) {
		var answer string
		var ok bool
		cluster.Read(func(current *state.State) { answer, ok = current.Challenges[r.PathValue("token")] })
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, answer) // the caller is gone if this fails
	})
	return mux
}

// serveChallenges serves challengeHandler on 127.0.0.1, where Traefik's
// challenge router sends the requests. The loopback address always exists,
// so the listener needs no freeBind.
func serveChallenges(cluster loop.Cluster, logger *slog.Logger) (*http.Server, error) {
	listener, err := net.Listen("tcp", ingress.ChallengeAddress)
	if err != nil {
		return nil, fmt.Errorf("listen for ACME's challenges on %s: %w", ingress.ChallengeAddress, err)
	}
	server := &http.Server{
		Handler:           challengeHandler(cluster),
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}
	go func() {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve ACME's challenges", "error", err)
		}
	}()
	return server, nil
}
