package vault

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	mathrand "math/rand"
	"net"
	"net/http"
	"time"

	log "github.com/mgutz/logxi/v1"

	"golang.org/x/net/http2"

	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/go-uuid"
	"github.com/hashicorp/vault/helper/forwarding"
	"github.com/hashicorp/vault/helper/jsonutil"
)

const (
	// Storage path where the local cluster name and identifier are stored
	coreLocalClusterInfoPath = "core/cluster/local/info"

	corePrivateKeyTypeP521    = "p521"
	corePrivateKeyTypeED25519 = "ed25519"

	// Internal so as not to log a trace message
	IntNoForwardingHeaderName = "X-Vault-Internal-No-Request-Forwarding"
)

var (
	ErrCannotForward = errors.New("cannot forward request; no connection or address not known")
)

// This can be one of a few key types so the different params may or may not be filled
type clusterKeyParams struct {
	Type string   `json:"type" structs:"type" mapstructure:"type"`
	X    *big.Int `json:"x" structs:"x" mapstructure:"x"`
	Y    *big.Int `json:"y" structs:"y" mapstructure:"y"`
	D    *big.Int `json:"d" structs:"d" mapstructure:"d"`
}

type activeConnection struct {
	transport   *http2.Transport
	clusterAddr string
}

// Structure representing the storage entry that holds cluster information
type Cluster struct {
	// Name of the cluster
	Name string `json:"name" structs:"name" mapstructure:"name"`

	// Identifier of the cluster
	ID string `json:"id" structs:"id" mapstructure:"id"`
}

// Cluster fetches the details of the local cluster. This method errors out
// when Vault is sealed.
func (c *Core) Cluster() (*Cluster, error) {
	var cluster Cluster

	// Fetch the storage entry. This call fails when Vault is sealed.
	entry, err := c.barrier.Get(coreLocalClusterInfoPath)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return &cluster, nil
	}

	// Decode the cluster information
	if err = jsonutil.DecodeJSON(entry.Value, &cluster); err != nil {
		return nil, fmt.Errorf("failed to decode cluster details: %v", err)
	}

	// Set in config file
	if c.clusterName != "" {
		cluster.Name = c.clusterName
	}

	return &cluster, nil
}

// This sets our local cluster cert and private key based on the advertisement.
// It also ensures the cert is in our local cluster cert pool.
func (c *Core) loadLocalClusterTLS(adv activeAdvertisement) error {
	switch {
	case adv.ClusterAddr == "":
		// Clustering disabled on the server, don't try to look for params
		return nil

	case adv.ClusterKeyParams == nil:
		c.logger.Error("core: no key params found loading local cluster TLS information")
		return fmt.Errorf("no local cluster key params found")

	case adv.ClusterKeyParams.X == nil, adv.ClusterKeyParams.Y == nil, adv.ClusterKeyParams.D == nil:
		c.logger.Error("core: failed to parse local cluster key due to missing params")
		return fmt.Errorf("failed to parse local cluster key")

	case adv.ClusterKeyParams.Type != corePrivateKeyTypeP521:
		c.logger.Error("core: unknown local cluster key type", "key_type", adv.ClusterKeyParams.Type)
		return fmt.Errorf("failed to find valid local cluster key type")

	case adv.ClusterCert == nil || len(adv.ClusterCert) == 0:
		c.logger.Error("core: no local cluster cert found")
		return fmt.Errorf("no local cluster cert found")

	}

	// Prevent data races with the TLS parameters
	c.clusterParamsLock.Lock()
	defer c.clusterParamsLock.Unlock()

	c.localClusterPrivateKey = &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{
			Curve: elliptic.P521(),
			X:     adv.ClusterKeyParams.X,
			Y:     adv.ClusterKeyParams.Y,
		},
		D: adv.ClusterKeyParams.D,
	}

	c.localClusterCert = adv.ClusterCert

	cert, err := x509.ParseCertificate(c.localClusterCert)
	if err != nil {
		c.logger.Error("core: failed parsing local cluster certificate", "error", err)
		return fmt.Errorf("error parsing local cluster certificate: %v", err)
	}

	c.clusterCertPool.AddCert(cert)

	return nil
}

// setupCluster creates storage entries for holding Vault cluster information.
// Entries will be created only if they are not already present. If clusterName
// is not supplied, this method will auto-generate it.
func (c *Core) setupCluster() error {
	// Prevent data races with the TLS parameters
	c.clusterParamsLock.Lock()
	defer c.clusterParamsLock.Unlock()

	// Check if storage index is already present or not
	cluster, err := c.Cluster()
	if err != nil {
		c.logger.Error("core: failed to get cluster details", "error", err)
		return err
	}

	var modified bool

	if cluster == nil {
		cluster = &Cluster{}
	}

	if cluster.Name == "" {
		// If cluster name is not supplied, generate one
		if c.clusterName == "" {
			c.logger.Trace("core: cluster name not found/set, generating new")
			clusterNameBytes, err := uuid.GenerateRandomBytes(4)
			if err != nil {
				c.logger.Error("core: failed to generate cluster name", "error", err)
				return err
			}

			c.clusterName = fmt.Sprintf("vault-cluster-%08x", clusterNameBytes)
		}

		cluster.Name = c.clusterName
		if c.logger.IsDebug() {
			c.logger.Debug("core: cluster name set", "name", cluster.Name)
		}
		modified = true
	}

	if cluster.ID == "" {
		c.logger.Trace("core: cluster ID not found, generating new")
		// Generate a clusterID
		cluster.ID, err = uuid.GenerateUUID()
		if err != nil {
			c.logger.Error("core: failed to generate cluster identifier", "error", err)
			return err
		}
		if c.logger.IsDebug() {
			c.logger.Debug("core: cluster ID set", "id", cluster.ID)
		}
		modified = true
	}

	// If we're using HA, generate server-to-server parameters
	if c.ha != nil {
		// Create a private key
		{
			c.logger.Trace("core: generating cluster private key")
			key, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
			if err != nil {
				c.logger.Error("core: failed to generate local cluster key", "error", err)
				return err
			}

			c.localClusterPrivateKey = key
		}

		// Create a certificate
		{
			c.logger.Trace("core: generating local cluster certificate")

			host, err := uuid.GenerateUUID()
			if err != nil {
				return err
			}
			host = fmt.Sprintf("fw-%s", host)
			template := &x509.Certificate{
				Subject: pkix.Name{
					CommonName: host,
				},
				DNSNames: []string{host},
				ExtKeyUsage: []x509.ExtKeyUsage{
					x509.ExtKeyUsageServerAuth,
					x509.ExtKeyUsageClientAuth,
				},
				KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageKeyAgreement | x509.KeyUsageCertSign,
				SerialNumber: big.NewInt(mathrand.Int63()),
				NotBefore:    time.Now().Add(-30 * time.Second),
				// 30 years of single-active uptime ought to be enough for anybody
				NotAfter:              time.Now().Add(262980 * time.Hour),
				BasicConstraintsValid: true,
				IsCA: true,
			}

			certBytes, err := x509.CreateCertificate(rand.Reader, template, template, c.localClusterPrivateKey.Public(), c.localClusterPrivateKey)
			if err != nil {
				c.logger.Error("core: error generating self-signed cert", "error", err)
				return errwrap.Wrapf("unable to generate local cluster certificate: {{err}}", err)
			}

			_, err = x509.ParseCertificate(certBytes)
			if err != nil {
				c.logger.Error("core: error parsing self-signed cert", "error", err)
				return errwrap.Wrapf("error parsing generated certificate: {{err}}", err)
			}

			c.localClusterCert = certBytes
		}
	}

	if modified {
		// Encode the cluster information into as a JSON string
		rawCluster, err := json.Marshal(cluster)
		if err != nil {
			c.logger.Error("core: failed to encode cluster details", "error", err)
			return err
		}

		// Store it
		err = c.barrier.Put(&Entry{
			Key:   coreLocalClusterInfoPath,
			Value: rawCluster,
		})
		if err != nil {
			c.logger.Error("core: failed to store cluster details", "error", err)
			return err
		}
	}

	return nil
}

// SetClusterSetupFuncs sets the handler setup func
func (c *Core) SetClusterSetupFuncs(handler func() (http.Handler, http.Handler)) {
	c.clusterHandlerSetupFunc = handler
}

// startClusterListener starts cluster request listeners during postunseal. It
// is assumed that the state lock is held while this is run. Right now this
// only starts forwarding listeners; it's TBD whether other request types will
// be built in the same mechanism or started independently.
func (c *Core) startClusterListener() error {
	if c.clusterHandlerSetupFunc == nil {
		c.logger.Error("core: cluster handler setup function has not been set when trying to start listeners")
		return fmt.Errorf("cluster handler setup function has not been set")
	}

	if c.clusterAddr == "" {
		c.logger.Info("core: clustering disabled, not starting listeners")
		return nil
	}

	if c.clusterListenerAddrs == nil || len(c.clusterListenerAddrs) == 0 {
		c.logger.Warn("core: clustering not disabled but no addresses to listen on")
		return fmt.Errorf("cluster addresses not found")
	}

	c.logger.Trace("core: starting cluster listeners")

	err := c.startForwarding()
	if err != nil {
		return err
	}

	return nil
}

// stopClusterListener stops any existing listeners during preseal. It is
// assumed that the state lock is held while this is run.
func (c *Core) stopClusterListener() {
	if c.clusterAddr == "" {
		c.logger.Trace("core: clustering disabled, not stopping listeners")
		return
	}

	if !c.clusterListenersRunning {
		c.logger.Info("core: cluster listeners not running")
		return
	}
	c.logger.Info("core: stopping cluster listeners")

	// Tell the goroutine managing the listeners to perform the shutdown
	// process
	c.clusterListenerShutdownCh <- struct{}{}

	// The reason for this loop-de-loop is that we may be unsealing again
	// quickly, and if the listeners are not yet closed, we will get socket
	// bind errors. This ensures proper ordering.
	c.logger.Trace("core: waiting for success notification while stopping cluster listeners")
	<-c.clusterListenerShutdownSuccessCh
	c.clusterListenersRunning = false

	c.logger.Info("core: cluster listeners successfully shut down")
}

// ClusterTLSConfig generates a TLS configuration based on the local/replicated
// cluster key and cert.
func (c *Core) ClusterTLSConfig() (*tls.Config, error) {
	cluster, err := c.Cluster()
	if err != nil {
		return nil, err
	}
	if cluster == nil {
		return nil, fmt.Errorf("local cluster information is nil")
	}

	// Prevent data races with the TLS parameters
	c.clusterParamsLock.Lock()
	defer c.clusterParamsLock.Unlock()

	forwarding := c.localClusterCert != nil && len(c.localClusterCert) > 0

	var parsedCert *x509.Certificate
	if forwarding {
		parsedCert, err = x509.ParseCertificate(c.localClusterCert)
		if err != nil {
			return nil, fmt.Errorf("error parsing local cluster certificate: %v", err)
		}

		// This is idempotent, so be sure it's been added
		c.clusterCertPool.AddCert(parsedCert)
	}

	nameLookup := func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		c.clusterParamsLock.RLock()
		defer c.clusterParamsLock.RUnlock()

		if forwarding && clientHello.ServerName == parsedCert.Subject.CommonName {
			return &tls.Certificate{
				Certificate: [][]byte{c.localClusterCert},
				PrivateKey:  c.localClusterPrivateKey,
			}, nil
		}

		return nil, nil
	}

	var clientCertificates []tls.Certificate
	if forwarding {
		clientCertificates = append(clientCertificates, tls.Certificate{
			Certificate: [][]byte{c.localClusterCert},
			PrivateKey:  c.localClusterPrivateKey,
		})
	}

	tlsConfig := &tls.Config{
		// We need this here for the client side
		Certificates:   clientCertificates,
		RootCAs:        c.clusterCertPool,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      c.clusterCertPool,
		GetCertificate: nameLookup,
		MinVersion:     tls.VersionTLS12,
	}
	if forwarding {
		tlsConfig.ServerName = parsedCert.Subject.CommonName
	}

	return tlsConfig, nil
}

func (c *Core) SetClusterListenerAddrs(addrs []*net.TCPAddr) {
	c.clusterListenerAddrs = addrs
}

// WrapHandlerForClustering takes in Vault's HTTP handler and returns a setup
// function that returns both the original handler and one wrapped with cluster
// methods
func WrapHandlerForClustering(handler http.Handler, logger log.Logger) func() (http.Handler, http.Handler) {
	return func() (http.Handler, http.Handler) {
		// This mux handles cluster functions (right now, only forwarded requests)
		mux := http.NewServeMux()
		mux.HandleFunc("/cluster/local/forwarded-request", func(w http.ResponseWriter, req *http.Request) {
			freq, err := forwarding.ParseForwardedHTTPRequest(req)
			if err != nil {
				if logger != nil {
					logger.Error("http/forwarded-request-server: error parsing forwarded request", "error", err)
				}

				w.Header().Add("Content-Type", "application/json")

				// The response writer here is different from
				// the one set in Vault's HTTP handler.
				// Hence, set the Cache-Control explicitly.
				w.Header().Set("Cache-Control", "no-store")

				w.WriteHeader(http.StatusInternalServerError)

				type errorResponse struct {
					Errors []string
				}
				resp := &errorResponse{
					Errors: []string{
						err.Error(),
					},
				}

				enc := json.NewEncoder(w)
				enc.Encode(resp)
				return
			}

			// To avoid the risk of a forward loop in some pathological condition,
			// set the no-forward header
			freq.Header.Set(IntNoForwardingHeaderName, "true")
			handler.ServeHTTP(w, freq)
		})

		return handler, mux
	}
}