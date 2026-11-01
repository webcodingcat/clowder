package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/webcodingcat/clowder/internal/pki"
	"github.com/webcodingcat/clowder/internal/recoverycode"
	"github.com/webcodingcat/clowder/internal/state"
)

// Access comes back only with a code from root on a host
// (clowderd user recovery-code, internal/recoverycode). Every agent checks
// a code alone, so nothing reaches the Raft log before the proof.
//
//  1. GET /v1/recover/offer?user=<name>: the agent sends its CA certificate
//     and, for each window whose code it takes, pki.TrustProof made with
//     that code. The CLI trusts the CA only if one proof matches the code
//     typed in. The answer is the same for a user that does not exist.
//  2. POST /v1/recover over TLS checked against that CA: the user, a new
//     public key and the code. The agent checks the code, issues the
//     certificate and commits recover-user, which revokes every other
//     certificate of the user and records the code as used: a code opens
//     once, so whoever saw it cannot lock the user out with it after.
//
// Both come without a certificate, so the agent takes only so many tries
// (limiter). POST /v1/recovery/rotate is the other use of a code: with the
// user's certificate and the user's own code for it, a new recovery key.

// RecoverOffer is the agent's answer to the first contact of a recovery.
// It comes unchecked; the proofs make the CA trustworthy.
type RecoverOffer struct {
	CACertificate string   `json:"ca_certificate"`
	Proofs        [][]byte `json:"proofs"`
}

// RecoverRequest asks for a new certificate for a user, with a code.
type RecoverRequest struct {
	User      string `json:"user"`
	PublicKey []byte `json:"public_key"` // Ed25519
	Code      string `json:"code"`
}

// RecoverAnswer is the recovered user's new certificate.
type RecoverAnswer struct {
	Certificate string `json:"certificate"`
}

// RotateRequest asks for a new recovery key, with the caller's own code.
type RotateRequest struct {
	Recipient string `json:"recipient"`
	Code      string `json:"code"`
}

// errCodeRefused is the one answer to a code that does not open: a user
// that does not exist gets it too, so the routes tell nobody which users
// there are.
var errCodeRefused = errors.New("the code does not fit: wrong, expired, used already, or for another user")

// RecoveringCluster is a cluster the CLI found and trusts: its CA proved it
// knows the code root on a host made.
type RecoveringCluster struct {
	seed, version, user string
	code                recoverycode.Code
	CA                  *x509.Certificate
	CAPEM               string
}

// FindClusterByCode asks the agents at seeds in turn for their CA, until
// one answers, and trusts it only if it proves it knows the user's code.
func FindClusterByCode(ctx context.Context, seeds []string, user string, code recoverycode.Code,
	version string,
) (*RecoveringCluster, error) {
	var unreachable []string
	for _, seed := range seeds {
		address := "https://" + Address(seed) + "/v1/recover/offer?user=" + url.QueryEscape(user)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set(VersionHeader, version)
		var offer RecoverOffer
		err = send(uncheckedClient(), request, &offer)
		var statusError *StatusError
		switch {
		case errors.As(err, &statusError):
			return nil, fmt.Errorf("ask %s for its CA: %w", seed, err)
		case err != nil:
			unreachable = append(unreachable, fmt.Sprintf("%s: %v", seed, err))
			continue
		}
		if !slices.ContainsFunc(offer.Proofs, func(proof []byte) bool {
			return pki.CheckTrustProof(code[:], []byte(offer.CACertificate), proof)
		}) {
			return nil, fmt.Errorf("%s cannot prove it knows the code: a wrong or expired code, "+
				"one for another user, or not your cluster", seed)
		}
		ca, err := pki.ParseCertificate([]byte(offer.CACertificate))
		if err != nil {
			return nil, err
		}
		return &RecoveringCluster{
			seed: seed, version: version, user: user, code: code, CA: ca, CAPEM: offer.CACertificate,
		}, nil
	}
	return nil, fmt.Errorf("no agent answered:\n  %s", strings.Join(unreachable, "\n  "))
}

// Recover gets a new certificate (PEM) for the user, over TLS checked
// against the trusted CA. The cluster revokes the user's other
// certificates with it.
func (c *RecoveringCluster) Recover(ctx context.Context, public ed25519.PublicKey) (string, error) {
	body, err := json.Marshal(RecoverRequest{User: c.user, PublicKey: public, Code: c.code.String()})
	if err != nil {
		return "", err
	}
	address := "https://" + Address(c.seed) + "/v1/recover"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set(VersionHeader, c.version)
	var answer RecoverAnswer
	if err := send(checkedClient(c.CA), request, &answer); err != nil {
		return "", fmt.Errorf("get a new certificate from %s: %w", c.seed, err)
	}
	return answer.Certificate, nil
}

// recoverOffer answers the first contact of a recovery, the same for every
// user name: the codes come from the name alone.
func (s *Server) recoverOffer(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user")
	if err := pki.CheckUserName(user); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var caPEM []byte
	s.config.Cluster.Read(func(current *state.State) {
		if current.Keys != nil {
			caPEM = slices.Clone(current.Keys.CACertificate)
		}
	})
	if caPEM == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("the cluster is not bootstrapped yet"))
		return
	}
	key, err := s.config.Keys.Derive(recoverycode.Purpose)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	offer := RecoverOffer{CACertificate: string(caPEM)}
	for _, code := range recoverycode.Codes(key, recoverycode.ForRecover, user, time.Now()) {
		offer.Proofs = append(offer.Proofs, pki.TrustProof(code[:], caPEM))
	}
	writeJSON(w, http.StatusOK, offer)
}

// recoverUser issues a user's new certificate for a code and has every
// other certificate of the user revoked.
func (s *Server) recoverUser(w http.ResponseWriter, r *http.Request) {
	var request RecoverRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := pki.CheckUserName(request.User); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(request.PublicKey) != ed25519.PublicKeySize {
		writeError(w, http.StatusBadRequest, errors.New("the public key is no Ed25519 key"))
		return
	}
	code, err := recoverycode.Parse(request.Code)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	used, valid, err := s.checkCode(recoverycode.ForRecover, request.User, code)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var known bool
	s.config.Cluster.Read(func(current *state.State) { _, known = current.Users[request.User] })
	if !valid || !known {
		s.config.Logger.Warn("recovery refused", "user", request.User, "from", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, errCodeRefused)
		return
	}
	ca, err := s.authority()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	certificatePEM, err := ca.IssueUser(ed25519.PublicKey(request.PublicKey), request.User, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	certificate, err := pki.ParseCertificate(certificatePEM)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// the user acts, as the bootstrap's first user does: the code proved
	// it, and the command's kind tells the audit that it was a recovery.
	// No time here: the leader stamps its own (proposeHere).
	cmd, err := state.NewCommand(state.KindRecoverUser, time.Time{}, state.UserActor(request.User),
		state.RecoverUser{
			Name: request.User, Serial: pki.Serial(certificate), NotAfter: certificate.NotAfter, Code: used,
		})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, ok := s.commit(w, r, cmd, s.Propose); ok {
		s.config.Logger.Info("user recovered", "user", request.User, "from", r.RemoteAddr)
		writeJSON(w, http.StatusOK, RecoverAnswer{Certificate: string(certificatePEM)})
	}
}

// rotateRecovery puts a new recovery key in place: the caller's
// certificate alone is not enough, it takes the caller's own code too, so
// stolen credentials cannot swap the key.
func (s *Server) rotateRecovery(w http.ResponseWriter, r *http.Request) {
	var request RotateRequest
	if err := decodeStrict(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	code, err := recoverycode.Parse(request.Code)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	used, valid, err := s.checkCode(recoverycode.ForRotate, identityOf(r).Name, code)
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
		return
	case !valid:
		writeError(w, http.StatusUnauthorized, errCodeRefused)
		return
	}
	if _, err := age.ParseX25519Recipient(request.Recipient); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the recovery key's recipient: %w", err))
		return
	}
	s.write(w, r, state.KindSetRecoveryRecipient,
		state.SetRecoveryRecipient{Recipient: request.Recipient, Code: used})
}

// checkCode tells whether a code is the user's for the use now and was not
// used yet, and names it for the command that uses it. The state refuses
// the command too if the code was used in the meantime, through another
// agent.
func (s *Server) checkCode(use recoverycode.Use, user string, code recoverycode.Code) (state.UsedCode, bool, error) {
	key, err := s.config.Keys.Derive(recoverycode.Purpose)
	if err != nil {
		return state.UsedCode{}, false, err
	}
	expires, valid := recoverycode.Valid(key, use, user, code, time.Now())
	used := state.UsedCode{Digest: code.Digest(), Expires: expires}
	var usedBefore bool
	s.config.Cluster.Read(func(current *state.State) { _, usedBefore = current.UsedCodes[used.Digest] })
	return used, valid && !usedBefore, nil
}
