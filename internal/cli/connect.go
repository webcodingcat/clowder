package cli

import (
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/webcodingcat/clowder/internal/api"
	"github.com/webcodingcat/clowder/internal/buildinfo"
	"github.com/webcodingcat/clowder/internal/clientconfig"
	"github.com/webcodingcat/clowder/internal/pki"
)

// renewWarning is how long before a CLI certificate expires every command
// starts to warn about it. Nothing renews the certificate, so this leaves
// time to get a new one from a host.
const renewWarning = 30 * 24 * time.Hour

// globals are the flags that every command shares.
type globals struct {
	config         string
	context        string
	connectTimeout time.Duration
	// warnings is where connect writes its warnings (the command's stderr)
	warnings io.Writer
}

func (g *globals) register(root *cobra.Command) {
	root.PersistentFlags().StringVar(&g.config, "config", "",
		"the CLI's credentials file (default: $"+clientconfig.EnvVar+" or ~/.clowder/config)")
	root.PersistentFlags().StringVar(&g.context, "context", "",
		"the context of the credentials file to use (default: its current context)")
	root.PersistentFlags().DurationVar(&g.connectTimeout, "connect-timeout", api.DefaultConnectTimeout,
		"how long to wait for a host to take the connection before the next is tried")
	// check before any command runs, because every command that asks a
	// host uses the timeout
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if g.connectTimeout <= 0 {
			return usage("--connect-timeout %s: give a positive duration like 5s", g.connectTimeout)
		}
		g.warnings = cmd.ErrOrStderr()
		return nil
	}
}

// readConfig reads the credentials file and returns it with its path.
func (g *globals) readConfig() (clientconfig.Config, string, error) {
	path, err := clientconfig.Path(g.config)
	if err != nil {
		return clientconfig.Config{}, "", err
	}
	config, err := clientconfig.Read(path)
	return config, path, err
}

// connect reads the context that the flags pick. It returns a client for
// the cluster's agents at the context's endpoints. It warns about a
// certificate that expires soon.
func (g *globals) connect() (*api.Client, error) {
	config, _, err := g.readConfig()
	if err != nil {
		return nil, err
	}
	cliContext, err := config.Context(g.context)
	if err != nil {
		return nil, withHint(err)
	}
	certificate, err := tls.X509KeyPair([]byte(cliContext.Certificate), []byte(cliContext.Key))
	if err != nil {
		return nil, fmt.Errorf("the credentials of context %s: %w", cliContext.Name, err)
	}
	ca, err := pki.ParseCertificate([]byte(cliContext.CA))
	if err != nil {
		return nil, fmt.Errorf("the credentials of context %s: %w", cliContext.Name, err)
	}
	warnings := g.warnings
	if warnings == nil { // a caller that did not run the root's PersistentPreRunE
		warnings = os.Stderr
	}
	// X509KeyPair parses the leaf certificate into Leaf
	warnBeforeTheEnd(warnings, cliContext.Name, certificate.Leaf.NotAfter, time.Now())
	return api.NewClient(cliContext.Endpoints, g.connectTimeout, certificate, ca, buildinfo.Version), nil
}

// warnBeforeTheEnd warns that a context's certificate is about to expire,
// starting renewWarning before it does. An agent refuses the certificate
// once it has expired. A warning that cannot be written stops no command.
func warnBeforeTheEnd(out io.Writer, contextName string, notAfter, now time.Time) {
	left := notAfter.Sub(now)
	if left > renewWarning {
		return
	}
	var when string
	switch {
	case left <= 0:
		when = "ended on " + notAfter.Format(time.DateOnly)
	case left < 24*time.Hour:
		when = "ends within a day, on " + notAfter.Format(time.DateOnly)
	default:
		when = fmt.Sprintf("ends on %s, in %d days", notAfter.Format(time.DateOnly), int(left.Hours()/24))
	}
	_, _ = fmt.Fprintf(out, "warning: the CLI certificate of context %s %s; get a new one from a host: "+
		"%s --as %s --replace\n", contextName, when, fromAHost, contextName)
}
