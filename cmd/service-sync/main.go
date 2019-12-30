package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"pack.ag/amqp"
	"strconv"
	"strings"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	//dockerfilters "github.com/docker/docker/api/types/filters"
	dockermounttypes "github.com/docker/docker/api/types/mount"
	dockernetworktypes "github.com/docker/docker/api/types/network"
	//dockerapi "github.com/docker/docker/client"

	"github.com/ajssmith/skupper-docker/pkg/dockershim/libdocker"
)

const (
	ServiceSyncAddress = "mc/$skupper-service-sync"
)

var (
	hostPath        = "/tmp/skupper"
	skupperCertPath = hostPath + "/qpid-dispatch-certs/"
)

func getTlsConfig(verify bool, cert, key, ca string) (*tls.Config, error) {
	var config tls.Config
	config.InsecureSkipVerify = true
	if verify {
		certPool := x509.NewCertPool()
		file, err := ioutil.ReadFile(ca)
		if err != nil {
			return nil, err
		}
		certPool.AppendCertsFromPEM(file)
		config.RootCAs = certPool
		config.InsecureSkipVerify = false
	}

	_, errCert := os.Stat(cert)
	_, errKey := os.Stat(key)
	if errCert == nil || errKey == nil {
		tlsCert, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			log.Fatal("Could not load x509 key pair", err.Error())
		}
		config.Certificates = []tls.Certificate{tlsCert}
	}
	config.MinVersion = tls.VersionTLS10

	return &config, nil
}

func authOption(username string, password string) amqp.ConnOption {
	if len(password) > 0 && len(username) > 0 {
		return amqp.ConnSASLPlain(username, password)
	} else {
		return amqp.ConnSASLAnonymous()
	}
}
func reconcile(service map[string]interface{}) {
	var update = false
	var port uint32
	var protocol string
	var targetPort uint32

	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)

	name := service["name"].(string)
	proxy := service["proxy"].(string)

	for _, bridge := range service["ports"].([]interface{}) {
		if bridge, ok := bridge.(map[string]interface{}); ok {
			port = bridge["port"].(uint32)
			protocol = bridge["protocol"].(string)
			targetPort = bridge["targetPort"].(uint32)
		}
	}

	fields := []string{}
	fields = append(fields, "port:"+strconv.Itoa(int(port)))
	fields = append(fields, "protocol:"+string(protocol))
	fields = append(fields, "targetPort:"+strconv.Itoa(int(targetPort)))
	label := strings.Join(fields, ",")

	labels := map[string]string{
		"application":          "skupper-proxy",
		"skupper.io/component": "proxy",
		"skupper.io/service":   label,
	}

	current, err := dd.InspectContainer(name)
	if err == nil {
		if val, ok := current.Config.Labels["skupper.io/service"]; ok {
			if val != label {
				// delete then update
				fmt.Printf("Detected proxy config update for %s: %s\n", name, val)
				undeploy(name, dd)
				update = true
			} else {
				fmt.Printf("No update required for proxy container %s\n", name)
			}
		} // else why doesn't container have label?
	} else {
		update = true
	}

	if update {
		deploy(name, proxy, port, labels, dd)
	}
}

func undeploy(name string, dd libdocker.Interface) {

	err := dd.StopContainer(name, 10*time.Second)
	if err == nil {
		if err := dd.RemoveContainer(name, dockertypes.ContainerRemoveOptions{}); err != nil {
			log.Fatal("Failed to remove proxy container: ", err.Error())
		}
	}
}

func deploy(name string, proxy string, port uint32, labels map[string]string, dd libdocker.Interface) {

	log.Println("Attach to VAN: ", labels["skupper.io/service"])

	var imageName string
	if os.Getenv("PROXY_IMAGE") != "" {
		imageName = os.Getenv("PROXY_IMAGE")
	} else {
		imageName = "quay.io/ajssmith/icproxy-simple"
	}
	err := dd.PullImage(imageName, dockertypes.AuthConfig{}, dockertypes.ImagePullOptions{})
	if err != nil {
		log.Fatal("Failed to pull proxy image: ", err.Error())
	}

	//labels := getLabels("proxy", bridge)
	bridges := []string{}

	//bridges = append(bridges, "amqp:"+name+"=>"+proxy+":"+strconv.Itoa(int(port)))
	// This is for incoming, e.g. no local process for the service
	bridges = append(bridges, proxy+":"+strconv.Itoa(int(port))+"=>amqp:"+name)
	bridgeCfg := strings.Join(bridges, ",")

	containerCfg := &dockercontainer.Config{
		Hostname: name,
		Image:    imageName,
		Cmd: []string{
			"node",
			"/opt/app-root/bin/simple.js",
			bridgeCfg},
		Env:    []string{"ICPROXY_BRIDGE_HOST=" + name},
		Labels: labels,
	}
	hostCfg := &dockercontainer.HostConfig{
		Mounts: []dockermounttypes.Mount{
			{
				Type:   dockermounttypes.TypeBind,
				Source: skupperCertPath + "skupper",
				Target: "/etc/messaging",
			},
		},
		Privileged: true,
	}
	networkCfg := &dockernetworktypes.NetworkingConfig{
		EndpointsConfig: map[string]*dockernetworktypes.EndpointSettings{
			"skupper-network": {},
		},
	}

	opts := &dockertypes.ContainerCreateConfig{
		Name:             name,
		Config:           containerCfg,
		HostConfig:       hostCfg,
		NetworkingConfig: networkCfg,
	}
	_, err = dd.CreateContainer(*opts)
	if err != nil {
		log.Fatal("Failed to create proxy container: ", err.Error())
	}
	err = dd.StartContainer(opts.Name)
	if err != nil {
		log.Fatal("Failed to start proxy container: ", err.Error())
	}

}

func syncSender(s *amqp.Session) {
	var request amqp.Message
	var properties amqp.MessageProperties

	ctx := context.Background()
	sender, err := s.NewSender(
		amqp.LinkTargetAddress(ServiceSyncAddress),
	)
	if err != nil {
		log.Fatal("Failed to create sender:", err)
	}

	defer func() {
		sender.Close(ctx)
	}()

	ticker := time.NewTicker(10 * time.Second)

	properties.Subject = "service-sync-request"
	request.Properties = &properties
	request.Value = ""
	for {
		select {
		case t := <-ticker.C:
			log.Println("Skupper service sync sending request at t", t)
			err = sender.Send(ctx, &request)
			log.Println("Skupper service request sent")
		}
	}

}
func main() {
	log.Printf("Skupper service sync starting")

	config, err := getTlsConfig(true, "/etc/messaging/tls.crt", "/etc/messaging/tls.key", "/etc/messaging/ca.crt")

	client, err := amqp.Dial("amqps://skupper-router:5671", amqp.ConnSASLAnonymous(), amqp.ConnMaxFrameSize(4294967295), amqp.ConnTLSConfig(config))
	if err != nil {
		log.Fatal("Failed to connect to url", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		log.Fatal("Failed to create session:", err)
	}

	go syncSender(session)

	ctx := context.Background()
	receiver, err := session.NewReceiver(
		amqp.LinkSourceAddress(ServiceSyncAddress),
		amqp.LinkCredit(10),
	)
	if err != nil {
		log.Fatal("Failed to create receiver:", err.Error())
	}
	defer func() {
		ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
		receiver.Close(ctx)
		cancel()
	}()

	for {
		msg, err := receiver.Receive(ctx)
		if err != nil {
			log.Fatal("Reading message from service synch: ", err.Error())
		}
		// Decode message as it is either a request to send update
		// or it is a receipt that needs to be reconciled
		msg.Accept()
		subject := msg.Properties.Subject

		if subject == "service-sync-request" {
			log.Println("Received service sync request")
			// signal sender
		} else if subject == "service-sync-update" {
			log.Println("Received service sync update")
			if updates, ok := msg.Value.([]interface{}); ok {
				for _, update := range updates {
					if update, ok := update.(map[string]interface{}); ok {
						reconcile(update)
					}
				}
			} else {
				log.Println("Skupper service sync update type assertion error")
			}

			// if msg.ApplicationProperties.origin != origin {
			//log.Printf("Received service-sync-update from %s: %j\n", msg.ApplicationProperties.origin, msg.Value)

		} else {
			log.Println("Service sync subject not valid")
		}
	}

	log.Println("Skupper service sync terminating")

}
