// Copyright 2018 VMware, Inc.
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/ligato/networkservicemesh/controlplane/pkg/apis/local/networkservice"
	"github.com/ligato/networkservicemesh/controlplane/pkg/apis/registry"
	"github.com/ligato/networkservicemesh/controlplane/pkg/nsmd"
	"github.com/ligato/networkservicemesh/pkg/tools"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

const (
	// NetworkServiceName defines Network Service Name the NSE is serving for
	advertiseNseNameEnv     = "ADVERTISE_NSE_NAME"
	advertiseNseLabelsEnv   = "ADVERTISE_NSE_LABELS"
	outgoingNscNameEnv      = "OUTGOING_NSC_NAME"
	outgoingNscLabelsEnv    = "OUTGOING_NSC_LABELS"
	DefaultVPPAgentEndpoint = "localhost:9112"
)

func main() {
	// Capture signals to cleanup before exiting
	c := make(chan os.Signal, 1)
	signal.Notify(c,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)

	nsmServerSocket := getEnv(nsmd.NsmServerSocketEnv, "nsmServerSocket")
	nsmClientSocket := getEnv(nsmd.NsmClientSocketEnv, "nsmClientSocket")
	workspace := getEnv(nsmd.WorkspaceEnv, "workspace")
	advertiseNseName := getEnv(advertiseNseNameEnv, "Advertise Network Service Name")
	outgoingNscName := getEnv(outgoingNscNameEnv, "Outgoing Network Service Name")
	advertiseNseLabels := getEnv(advertiseNseLabelsEnv, "Advertise labels")
	outgoingNscLabels := getEnv(outgoingNscLabelsEnv, "Outgoing labels")

	// For NSE to program container's dataplane, container's linux namespace must be sent to NSM
	linuxNS, err := tools.GetCurrentNS()
	if err != nil {
		logrus.Fatalf("nse: failed to get a linux namespace with error: %v, exiting...", err)
	}
	logrus.Infof("Starting NSE, linux namespace: %s", linuxNS)

	if err := tools.SocketCleanup(nsmClientSocket); err != nil {
		logrus.Fatalf("nse: failure to cleanup stale socket %s with error: %v", nsmClientSocket, err)
	}

	logrus.Infof("nse: listening socket %s", nsmClientSocket)
	connectionServer, err := net.Listen("unix", nsmClientSocket)
	if err != nil {
		logrus.Fatalf("nse: failure to listen on a socket %s with error: %v", nsmClientSocket, err)
	}
	grpcServer := grpc.NewServer()

	// Check if the socket of Endpoint Connection Server is operation
	testSocket, err := tools.SocketOperationCheck(nsmServerSocket)
	if err != nil {
		logrus.Fatalf("nse: failure to communicate with the nsm on socket %s with error: %v", nsmServerSocket, err)
	}
	testSocket.Close()

	// NSE connection server is ready and now endpoints can be advertised to NSM

	if _, err := os.Stat(nsmServerSocket); err != nil {
		logrus.Fatalf("nse: failure to access nsm socket at %s with error: %+v, exiting...", nsmServerSocket, err)
	}

	conn, err := tools.SocketOperationCheck(nsmServerSocket)
	if err != nil {
		logrus.Fatalf("nse: failure to communicate with the registrySocket %s with error: %+v", nsmServerSocket, err)
	}
	defer conn.Close()
	logrus.Infof("nsm: connection to nsm server on socket: %s succeeded.", nsmServerSocket)

	registryConnection := registry.NewNetworkServiceRegistryClient(conn)
	clientConnection := networkservice.NewNetworkServiceClient(conn)

	// Registering NSE API, it will listen for Connection requests from NSM and return information
	// needed for NSE's dataplane programming.
	nseConn := New(outgoingNscName, DefaultVPPAgentEndpoint, workspace,
		tools.ParseKVStringToMap(outgoingNscLabels, ",", "="),
		clientConnection)
	networkservice.RegisterNetworkServiceServer(grpcServer, nseConn)

	go func() {
		if err := grpcServer.Serve(connectionServer); err != nil {
			logrus.Fatalf("nse: failed to start grpc server on socket %s with error: %v ", nsmClientSocket, err)
		}
	}()

	nse := &registry.NetworkServiceEndpoint{
		NetworkServiceName: advertiseNseName,
		Payload:            "IP",
		Labels:             tools.ParseKVStringToMap(advertiseNseLabels, ",", "="),
	}
	registration := &registry.NSERegistration{
		NetworkService: &registry.NetworkService{
			Name:    advertiseNseName,
			Payload: "IP",
		},
		NetworkserviceEndpoint: nse,
	}

	registeredNSE, err := registryConnection.RegisterNSE(context.Background(), registration)
	if err != nil {
		logrus.Fatalln("unable to register endpoint", err)
	}
	logrus.Infof("NSE registered: %v", registeredNSE)

	// prepare and defer removing of the advertised endpoint
	removeNSE := &registry.RemoveNSERequest{
		EndpointName: registeredNSE.GetNetworkserviceEndpoint().GetEndpointName(),
	}

	defer registryConnection.RemoveNSE(context.Background(), removeNSE)
	defer grpcServer.Stop()

	logrus.Infof("nse: channel has been successfully advertised, waiting for connection from NSM...")

	select {
	case <-c:
		logrus.Infof("Closing %v", advertiseNseName)
	}
}

func getEnv(key, description string) string {
	value, ok := os.LookupEnv(key)
	if !ok {
		logrus.Fatalf("Error getting %v: %v", key, ok)
	}
	logrus.Infof("%s: %s", description, value)
	return value
}
