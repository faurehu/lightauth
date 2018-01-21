package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	serverAddr         = flag.String("server_addr", "localhost:10001", "The lnd address")
	caFile             = flag.String("ca_file", "/home/faure/.lnd/tls.cert", "The file containing the CA root cert file")
	serverHostOverride = flag.String("server_host_override", "", "The server name use to verify the hostname returned by TLS handshake")
)

func main() {
	flag.Parse()
	var opts []grpc.DialOption

	creds, err := credentials.NewClientTLSFromFile("/home/faure/.lnd/tls.cert", "")
	if err != nil {
		log.Fatalf("Failed to create TLS credentials %v", err)
	}

	opts = append(opts, grpc.WithTransportCredentials(creds))

	conn, err := grpc.Dial(*serverAddr, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	client := lnrpc.NewLightningClient(conn)
	ctxb := context.Background()
	channelsResponse, err := client.GetInfo(ctxb, &lnrpc.GetInfoRequest{})
	if err != nil {
		log.Fatalf("error %v", err)
	}
	fmt.Print(channelsResponse)
}
