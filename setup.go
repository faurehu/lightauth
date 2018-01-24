package lightauth

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	store           = make(map[string]*route)
	conn            *grpc.ClientConn
	lightningClient lnrpc.LightningClient
)

type routeInfo struct {
	Name        string
	Fee         int
	MaxInvoices int
	Mode        string
	Period      string
}

type tomlConfig struct {
	ServerAddr         string
	CAFile             string
	ServerHostOverride string
	Routes             map[string]*routeInfo
}

// StartConnection is used to initiate the connection with the LDN node.
// It requires lightauth.toml to be populated with the connection params and
// the routes.
func StartConnection() *grpc.ClientConn {
	var conf tomlConfig
	if _, err := toml.DecodeFile("lightauth.toml", &conf); err != nil {
		fmt.Fprintf(os.Stderr, "error: Could not parse lightauth.toml %v\n", err)
		os.Exit(1)
	}

	var opts []grpc.DialOption

	creds, err := credentials.NewClientTLSFromFile(conf.CAFile, conf.ServerHostOverride)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: Failed to create TLS credentials %v\n", err)
		os.Exit(1)
	}

	opts = append(opts, grpc.WithTransportCredentials(creds))

	conn, err = grpc.Dial(conf.ServerAddr, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	for _, v := range conf.Routes {
		store[v.Name] = &route{
			clients: make(map[string]*client),
			routeInfo: routeInfo{
				Name:        v.Name,
				Fee:         v.Fee,
				MaxInvoices: v.MaxInvoices,
				Mode:        v.Mode,
				Period:      v.Period,
			},
		}
	}

	lightningClient = lnrpc.NewLightningClient(conn)

	// 	stream, err := lightningClient.SubscribeInvoices(context.Background, &lnrpc.InvoiceSubscription{})
	// 	if err != nil {
	// 		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	// 		os.Exit(1)
	// 	}

	// 	for {
	// 		invoiceUpdate, err := stream.Recv()
	// 		if err == io.EOF {
	// 			break
	// 		}
	// 		if err != nil {
	// 		}
	// 	}

	return conn
}
