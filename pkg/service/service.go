/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package service

import (
    "fmt"
    "strconv"
    "strings"
)

type Protocol string

const (
    ProtocolTCP   Protocol = "TCP"
    ProtocolUDP   Protocol = "UDP"
)

type ServicePort struct {
    Protocol   Protocol `json:"protocol"`
    Port       int32    `json:"port"`
    TargetPort int32    `json:"targetPort"`
}

type Proxy string

const (
    ProxyAMQP  Proxy = "AMQP"
    ProxyTCP   Proxy = "TCP"
    ProxyUDP   Proxy = "UDP"
    ProxyHTTP  Proxy = "HTTP"
    ProxyHTTP2 Proxy = "HTTP2"
)

type Service struct {
    Name    string        `json:"name"`
    Proxy   Proxy         `json:"proxy"`
    Ports   []ServicePort `json:"ports"`
    Process string        `json:"process"`
}

type ServiceList struct {
    Items []Service `json:"items"`
}

func (sp *ServicePort) String() string {
    return fmt.Sprintf("%s:%d", sp.Protocol, sp.Port)
}

func (sp *ServicePort) ServicePortLabel() string {
    fields := []string{}
    fields = append(fields, "port:"+strconv.Itoa(int(sp.Port)))
    fields = append(fields, "protocol:"+string(sp.Protocol))
    fields = append(fields, "targetPort:"+strconv.Itoa(int(sp.TargetPort)))
    return strings.Join(fields, ",")
}

