package lightauth

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/BurntSushi/toml"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	clientStore           = make(map[string]*path)
	serverStore           = make(map[string]*route)
	conn                  *grpc.ClientConn
	lightningClient       lnrpc.LightningClient
	lightningClientStream lnrpc.Lightning_SendPaymentClient
	lightningServerStream lnrpc.Lightning_SubscribeInvoicesClient
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

func startRPCClient() tomlConfig {
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

	lightningClient = lnrpc.NewLightningClient(conn)

	return conf
}

// StartClientConnection is used to initiate the connection with the LDN node on a client's behalf.
func StartClientConnection() *grpc.ClientConn {
	startRPCClient()

	ctxb := context.Background()
	var err error
	lightningClientStream, err = lightningClient.SendPayment(ctxb)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	go func() {
		for {
			paymentResponse, err := lightningClientStream.Recv()
			if err == io.EOF {
				return
			}

			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}

			if paymentResponse != nil {
				if paymentResponse.PaymentError != "" {
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
				} else {
					confirmInvoiceSettled(paymentResponse.PaymentPreimage)
				}
			}
		}
	}()

	return conn
}

// StartServerConnection is used to initiate the connection with the LDN node on a server's behalf.
// It requires lightauth.toml to be populated with the connection params and
// the routes.
func StartServerConnection() *grpc.ClientConn {
	conf := startRPCClient()

	for _, v := range conf.Routes {
		serverStore[v.Name] = &route{
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

	ctxb := context.Background()
	var err error
	lightningServerStream, err = lightningClient.SubscribeInvoices(ctxb, &lnrpc.InvoiceSubscription{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	go func() {
		for {
			invoiceUpdate, err := lightningServerStream.Recv()
			if err == io.EOF {
				return
			}

			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}

			if invoiceUpdate != nil && invoiceUpdate.Settled {
				updateInvoice(invoiceUpdate.PaymentRequest)
			}
		}
	}()

	return conn
}
