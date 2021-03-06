package k8s

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	// Load all the auth plugins for the cloud providers.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

// PortForward provides a port-forward connection into a Kubernetes cluster.
type PortForward struct {
	method     string
	url        *url.URL
	localPort  int
	remotePort int
	emitLogs   bool
	stopCh     chan struct{}
	readyCh    chan struct{}
	config     *rest.Config
}

// NewProxyMetricsForward returns an instance of the PortForward struct that can
// be used to establish a port-forward connection to a linkerd-proxy's metrics
// endpoint, specified by namespace and proxyPod.
func NewProxyMetricsForward(
	k8sAPI *KubernetesAPI,
	pod corev1.Pod,
	emitLogs bool,
) (*PortForward, error) {
	if pod.Status.Phase != corev1.PodRunning {
		return nil, fmt.Errorf("pod not running: %s", pod.GetName())
	}

	var container corev1.Container
	for _, c := range pod.Spec.Containers {
		if c.Name == ProxyContainerName {
			container = c
			break
		}
	}
	if container.Name != ProxyContainerName {
		return nil, fmt.Errorf("no %s container found for pod %s", ProxyContainerName, pod.GetName())
	}

	var port corev1.ContainerPort
	for _, p := range container.Ports {
		if p.Name == ProxyAdminPortName {
			port = p
			break
		}
	}
	if port.Name != ProxyAdminPortName {
		return nil, fmt.Errorf("no %s port found for container %s/%s", ProxyAdminPortName, pod.GetName(), container.Name)
	}

	return newPortForward(k8sAPI, pod.GetNamespace(), pod.GetName(), 0, int(port.ContainerPort), emitLogs)
}

// NewPortForward returns an instance of the PortForward struct that can be used
// to establish a port-forward connection to a pod in the deployment that's
// specified by namespace and deployName. If localPort is 0, it will use a
// random ephemeral port.
// Note that the connection remains open for the life of the process, as this
// function is typically called by the CLI. Care should be taken if called from
// control plane code.
func NewPortForward(
	k8sAPI *KubernetesAPI,
	namespace, deployName string,
	localPort, remotePort int,
	emitLogs bool,
) (*PortForward, error) {
	timeoutSeconds := int64(30)
	podList, err := k8sAPI.CoreV1().Pods(namespace).List(metav1.ListOptions{TimeoutSeconds: &timeoutSeconds})
	if err != nil {
		return nil, err
	}

	podName := ""
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning {
			if strings.HasPrefix(pod.Name, deployName) {
				podName = pod.Name
				break
			}
		}
	}

	if podName == "" {
		return nil, fmt.Errorf("no running pods found for %s", deployName)
	}

	return newPortForward(k8sAPI, namespace, podName, localPort, remotePort, emitLogs)
}

func newPortForward(
	k8sAPI *KubernetesAPI,
	namespace, podName string,
	localPort, remotePort int,
	emitLogs bool,
) (*PortForward, error) {

	req := k8sAPI.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward")

	var err error
	if localPort == 0 {
		localPort, err = getEphemeralPort()
		if err != nil {
			return nil, err
		}
	}

	return &PortForward{
		method:     "POST",
		url:        req.URL(),
		localPort:  localPort,
		remotePort: remotePort,
		emitLogs:   emitLogs,
		stopCh:     make(chan struct{}, 1),
		readyCh:    make(chan struct{}),
		config:     k8sAPI.Config,
	}, nil
}

// Run creates and runs the port-forward connection.
func (pf *PortForward) Run() error {
	transport, upgrader, err := spdy.RoundTripperFor(pf.config)
	if err != nil {
		return err
	}

	out := ioutil.Discard
	errOut := ioutil.Discard
	if pf.emitLogs {
		out = os.Stdout
		errOut = os.Stderr
	}

	ports := []string{fmt.Sprintf("%d:%d", pf.localPort, pf.remotePort)}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, pf.method, pf.url)

	fw, err := portforward.New(dialer, ports, pf.stopCh, pf.readyCh, out, errOut)
	if err != nil {
		return err
	}

	return fw.ForwardPorts()
}

// Ready returns a channel that will receive a message when the port-forward
// connection is ready. Clients should block and wait for the message before
// using the port-forward connection.
func (pf *PortForward) Ready() <-chan struct{} {
	return pf.readyCh
}

// Stop terminates the port-forward connection.
func (pf *PortForward) Stop() {
	close(pf.stopCh)
}

// URLFor returns the URL for the port-forward connection.
func (pf *PortForward) URLFor(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", pf.localPort, path)
}

// getEphemeralPort selects a port for the port-forwarding. It binds to a free
// ephemeral port and returns the port number.
func getEphemeralPort() (int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}

	defer ln.Close()

	// get port
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("invalid listen address: %s", ln.Addr())
	}

	return tcpAddr.Port, nil
}
