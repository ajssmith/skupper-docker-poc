package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"log"
	"os"
	"pack.ag/amqp"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	dockerfilters "github.com/docker/docker/api/types/filters"
	dockermounttypes "github.com/docker/docker/api/types/mount"
	dockernetworktypes "github.com/docker/docker/api/types/network"
	dockerapi "github.com/docker/docker/client"

	"../../pkg/dockershim/libdocker"
	//"github.com/ajssmith/skupper-docker/pkg/dockershim/libdocker"
)

const (
	ServiceSyncAddress = "mc/$skupper-service-sync"
)

var (
	hostPath                = "/tmp/skupper"
	skupperLocalBridgePath  = hostPath + "/local-bridges/"
	skupperRemoteBridgePath = hostPath + "/remote-bridges/"
)

type ProxyType string

const (
	ProxyTcp   ProxyType = "TCP"
	ProxyUdp             = "UDP"
	ProxyHttp            = "HTTP"
	ProxyHttp2           = "HTTP-2"
)

type ServiceType struct {
	Name   string
	Bridge ProxyBridge
	Proxy  string
}

type ProxyBridge struct {
	Port       uint32
	Protocol   ProxyType
	TargetPort uint32
}

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

func attachToVAN(bridge Bridge) bool {
	dd := libdocker.ConnectToDockerOrDie("", 0, 10*time.Second)

	if bridge.Process != "" {
		if bridge.Name == bridge.Process {
			fmt.Println("The bridge and process names must be unique")
			return false
		}
		_, err := dd.InspectContainer(bridge.Process)
		if err != nil {
			if dockerapi.IsErrNotFound(err) {
				fmt.Println("The process to bridge was not found (need docker run?)")
				return false
			} else {
				log.Fatal("Failed to retrieve process container: ", err.Error())
			}
		}
	}

	_, err := dd.InspectContainer(bridge.Name)
	if err == nil {
		fmt.Printf("A Bridge with the address %s already exists, please choose a different one", bridge.Name)
		fmt.Println()
	} else if dockerapi.IsErrNotFound(err) {
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

		labels := getLabels("proxy", bridge)
		bridges := []string{}
		bridges = append(bridges, "amqp:"+bridge.Name+"=>"+bridge.Protocol+":"+bridge.Port)
		if bridge.Process != "" {
			bridges = append(bridges, bridge.Protocol+":"+bridge.Port+"=>amqp:"+bridge.Name)
		}
		bridgeCfg := strings.Join(bridges, ",")

		containerCfg := &dockercontainer.Config{
			Hostname: bridge.Name,
			Image:    imageName,
			Cmd: []string{
				"node",
				"/opt/app-root/bin/simple.js",
				bridgeCfg},
			Env:    proxyEnv(bridge),
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
			Name:             bridge.Name,
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
	} else {
		log.Fatal("Failed to create proxy container: ", err.Error())
	}

	err = dd.ConnectContainerToNetwork("skupper-network", bridge.Process)
	if err != nil {
		log.Fatal("Failed to connect bridge process to skupper-network: ", err.Error())
	}
	return true
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
						var s ServiceType
						s.Name = update["name"].(string)
						s.Proxy = update["proxy"].(string)
						for _, bridge := range update["ports"].([]interface{}) {
							if bridge, ok := bridge.(map[string]interface{}); ok {
								s.Bridge.Port = bridge["port"].(uint32)
								s.Bridge.Protocol = ProxyType(bridge["protocol"].(string))
								s.Bridge.TargetPort = bridge["targetPort"].(uint32)
							}
						}
						// todo convert bridge to string and write to file
						log.Println("Service: ", s)
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
