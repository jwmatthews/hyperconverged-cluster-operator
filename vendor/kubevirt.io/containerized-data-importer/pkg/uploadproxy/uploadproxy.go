package uploadproxy

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	"kubevirt.io/containerized-data-importer/pkg/common"
	"kubevirt.io/containerized-data-importer/pkg/controller"
	"kubevirt.io/containerized-data-importer/pkg/token"
	"kubevirt.io/containerized-data-importer/pkg/uploadserver"
)

const (
	// selfsigned cert secret name
	apiCertSecretName = "cdi-api-certs"

	apiServiceName = "cdi-api"

	uploadPath  = "/v1alpha1/upload"
	healthzPath = "/healthz"

	connectTimeout = 2 * time.Second
	connectTries   = 5

	proxyRequestTimeout = time.Hour

	uploadTokenLeeway = 10 * time.Second
)

// Server is the public interface to the upload proxy
type Server interface {
	Start() error
}

type urlLookupFunc func(string, string) string

type uploadProxyApp struct {
	bindAddress string
	bindPort    uint

	client kubernetes.Interface

	certBytes []byte
	keyBytes  []byte

	tokenValidator token.Validator

	uploadServerClient *http.Client

	mux *http.ServeMux

	// test hook
	urlResolver urlLookupFunc
}

var authHeaderMatcher *regexp.Regexp

func init() {
	authHeaderMatcher = regexp.MustCompile(`(?i)^Bearer\s+([A-Za-z0-9\-\._~\+\/]+)$`)
}

// NewUploadProxy returns an initialized uploadProxyApp
func NewUploadProxy(bindAddress string,
	bindPort uint,
	apiServerPublicKey string,
	uploadClientKey string,
	uploadClientCert string,
	uploadServerCert string,
	serviceKey string,
	serviceCert string,
	client kubernetes.Interface) (Server, error) {
	var err error
	app := &uploadProxyApp{
		bindAddress: bindAddress,
		bindPort:    bindPort,
		client:      client,
		keyBytes:    []byte(serviceKey),
		certBytes:   []byte(serviceCert),
		urlResolver: uploadserver.GetUploadServerURL,
	}
	// retrieve RSA key used by apiserver to sign tokens
	err = app.getSigningKey(apiServerPublicKey)
	if err != nil {
		return nil, errors.Errorf("unable to retrieve apiserver signing key: %v", errors.WithStack(err))
	}

	// get upload server http client
	err = app.getUploadServerClient(uploadClientKey, uploadClientCert, uploadServerCert)
	if err != nil {
		return nil, errors.Errorf("unable to create upload server client: %v\n", errors.WithStack(err))
	}

	app.initHandlers()

	return app, nil
}

func (app *uploadProxyApp) getUploadServerClient(tlsClientKey, tlsClientCert, tlsServerCert string) error {
	clientCert, err := tls.X509KeyPair([]byte(tlsClientCert), []byte(tlsClientKey))
	if err != nil {
		return err
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM([]byte(tlsServerCert))

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caCertPool,
	}
	tlsConfig.BuildNameToCertificate()

	transport := &http.Transport{TLSClientConfig: tlsConfig}
	client := &http.Client{Transport: transport, Timeout: proxyRequestTimeout}

	app.uploadServerClient = client

	return nil
}

func (app *uploadProxyApp) initHandlers() {
	app.mux = http.NewServeMux()
	app.mux.HandleFunc(healthzPath, app.handleHealthzRequest)
	app.mux.HandleFunc(uploadPath, app.handleUploadRequest)
}

func (app *uploadProxyApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	app.mux.ServeHTTP(w, r)
}

func (app *uploadProxyApp) handleHealthzRequest(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "OK")
}

func (app *uploadProxyApp) handleUploadRequest(w http.ResponseWriter, r *http.Request) {
	tokenHeader := r.Header.Get("Authorization")
	if tokenHeader == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	match := authHeaderMatcher.FindStringSubmatch(tokenHeader)
	if len(match) != 2 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tokenData, err := app.tokenValidator.Validate(match[1])
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	if tokenData.Operation != token.OperationUpload ||
		tokenData.Name == "" ||
		tokenData.Namespace == "" ||
		tokenData.Resource.Resource != "persistentvolumeclaims" {
		klog.Errorf("Bad token %+v", tokenData)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	klog.V(1).Infof("Received valid token: pvc: %s, namespace: %s", tokenData.Name, tokenData.Namespace)

	err = app.uploadPossible(tokenData.Name, tokenData.Namespace)
	if err != nil {
		klog.Error(err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	app.proxyUploadRequest(tokenData.Namespace, tokenData.Name, w, r)
}

func (app *uploadProxyApp) uploadPossible(pvcName, pvcNamespace string) error {
	podName := controller.GetUploadResourceName(pvcName)
	pod, err := app.client.CoreV1().Pods(pvcNamespace).Get(podName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return fmt.Errorf("rejecting Upload Request for Pod %s that doesn't exist", podName)
		}
		return err
	}
	phase := pod.Status.Phase
	if phase == v1.PodSucceeded {
		return fmt.Errorf("rejecting Upload Request for Pod %s that already finished uploading", podName)
	}
	return nil
}

func (app *uploadProxyApp) proxyUploadRequest(namespace, pvc string, w http.ResponseWriter, r *http.Request) {
	url := app.urlResolver(namespace, pvc)

	if err := app.testConnect(url); err != nil {
		klog.Errorf("Error connecting to %s: %+v", url, err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	req, _ := http.NewRequest("POST", url, r.Body)
	req.ContentLength = r.ContentLength

	klog.V(3).Infof("Posting to: %s", url)

	response, err := app.uploadServerClient.Do(req)
	if err != nil {
		klog.Errorf("Error proxying %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	klog.V(3).Infof("Response status for url %s: %d", url, response.StatusCode)

	w.WriteHeader(response.StatusCode)
	_, err = io.Copy(w, response.Body)
	if err != nil {
		klog.Warningf("Error proxying response from url %s", url)
	}
}

func (app *uploadProxyApp) testConnect(urlString string) error {
	u, err := url.Parse(urlString)
	if err != nil {
		return errors.Wrapf(err, "error parsing URL %s", urlString)
	}

	port := u.Port()
	if port == "" {
		p, err := net.LookupPort("tcp", u.Scheme)
		if err != nil {
			return errors.Wrapf(err, "error looking up scheme %s", u.Scheme)
		}
		port = fmt.Sprintf("%d", p)
	}
	hostPort := net.JoinHostPort(u.Hostname(), port)

	for i := 1; i <= connectTries; i++ {
		d := net.Dialer{Timeout: connectTimeout}
		conn, err := d.Dial("tcp", hostPort)
		if err == nil {
			klog.V(3).Infof("Successfully connected to %s on attempt %d", urlString, i)
			conn.Close()
			return nil
		}

		switch ne := err.(type) {
		case net.Error:
			if ne.Timeout() {
				klog.V(3).Infof("Timeout connecting to %s on iteration %d", hostPort, i)
				continue
			}
			klog.V(3).Infof("Unexpected net error connecting to %s on iteration %d %+v", hostPort, i, err)
		default:
			klog.V(3).Infof("Unexpected error connecting to %s on iteration %d %+v", hostPort, i, err)
		}
	}

	return errors.Errorf("timed out %d times connecting to %s", connectTries, urlString)
}

func (app *uploadProxyApp) getSigningKey(publicKeyPEM string) error {
	publicKey, err := controller.DecodePublicKey([]byte(publicKeyPEM))
	if err != nil {
		return err
	}

	app.tokenValidator = token.NewValidator(common.UploadTokenIssuer, publicKey, uploadTokenLeeway)
	return nil
}

func (app *uploadProxyApp) Start() error {
	return app.startTLS()
}

func (app *uploadProxyApp) startTLS() error {
	var serveFunc func() error
	bindAddr := fmt.Sprintf("%s:%d", app.bindAddress, app.bindPort)

	server := &http.Server{
		Addr:    bindAddr,
		Handler: app,
	}

	if len(app.keyBytes) > 0 && len(app.certBytes) > 0 {
		certsDirectory, err := ioutil.TempDir("", "certsdir")
		if err != nil {
			return errors.Errorf("unable to create certs temporary directory: %v\n", errors.WithStack(err))
		}
		defer os.RemoveAll(certsDirectory)

		keyFile := filepath.Join(certsDirectory, "key.pem")
		certFile := filepath.Join(certsDirectory, "cert.pem")

		err = ioutil.WriteFile(keyFile, app.keyBytes, 0600)
		if err != nil {
			return err
		}
		err = ioutil.WriteFile(certFile, app.certBytes, 0600)
		if err != nil {
			return err
		}

		serveFunc = func() error {
			return server.ListenAndServeTLS(certFile, keyFile)
		}
	} else {
		serveFunc = func() error {
			return server.ListenAndServe()
		}
	}

	errChan := make(chan error)

	go func() {
		errChan <- serveFunc()
	}()

	// wait for server to exit
	return <-errChan
}
