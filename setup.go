package lightauth

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/BurntSushi/toml"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	clientStore           map[string]*Path
	serverStore           map[string]*Route
	conn                  *grpc.ClientConn
	lightningClient       lnrpc.LightningClient
	lightningClientStream lnrpc.Lightning_SendPaymentClient
	lightningServerStream lnrpc.Lightning_SubscribeInvoicesClient
	database              DataProvider
)

// Record is an interface that superclasses all entities stored in a permanent store
type Record interface {
	save() error
}

// DataProvider is an interface that specifies the methods required to store data
type DataProvider interface {
	Create(Record) (string, error)
	Edit(Record)
	GetServerData() (map[string]*Route, error)
	GetClientData() (map[string]*Path, error)
}

// RouteInfo is the bare fields that details a route
type RouteInfo struct {
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
	Routes             map[string]*RouteInfo
}

func startRPCClient() tomlConfig {
	var conf tomlConfig
	if _, err := toml.DecodeFile("lightauth.toml", &conf); err != nil {
		log.Fatalf("Lightauth error: Could not parse lightauth.toml: %v\n", err)
	}

	var opts []grpc.DialOption

	creds, err := credentials.NewClientTLSFromFile(conf.CAFile, conf.ServerHostOverride)
	if err != nil {
		log.Fatalf("Lightauth error: Failed to create TLS credentials: %v\n", err)
	}

	opts = append(opts, grpc.WithTransportCredentials(creds))

	conn, err = grpc.Dial(conf.ServerAddr, opts...)
	if err != nil {
		log.Fatalf("Lightauth error: Failed to start grpc connection: %v\n", err)
	}

	lightningClient = lnrpc.NewLightningClient(conn)

	return conf
}

// StartClientConnection is used to initiate the connection with the LDN node on a client's behalf.
func StartClientConnection(db DataProvider) *grpc.ClientConn {
	database = db
	startRPCClient()

	var err error
	clientStore, err = db.GetClientData()
	if err != nil {
		log.Fatalf("Lightauth error: could not fetch data from store: %v\n", err)
	}

	ctxb := context.Background()
	lightningClientStream, err = lightningClient.SendPayment(ctxb)
	if err != nil {
		log.Fatalf("Lightauth error: Failed to start lightning client stream: %v\n", err)
	}

	go func() {
		for {
			paymentResponse, err := lightningClientStream.Recv()
			if err == io.EOF {
				return
			}

			if err != nil {
				log.Fatalf("Lightauth error: There was an error receiving data from the lightning client stream: %v\n", err)
			}

			if paymentResponse != nil {
				if paymentResponse.PaymentError != "" {
					log.Printf("Lightauth error: Lightning payment contains an error: %v\n", paymentResponse.PaymentError)
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
func StartServerConnection(db DataProvider) *grpc.ClientConn {
	database = db
	conf := startRPCClient()

	var err error
	serverStore, err = db.GetServerData()
	if err != nil {
		log.Fatalf("Lightauth error: could not fetch data from store: %v\n", err)
	}

	for _, v := range conf.Routes {
		if _, exists := serverStore[v.Name]; !exists {
			// TODO: Delete from store those routes not in toml
			r := &Route{
				Clients: make(map[string]*Client),
				RouteInfo: RouteInfo{
					Name:        v.Name,
					Fee:         v.Fee,
					MaxInvoices: v.MaxInvoices,
					Mode:        v.Mode,
					Period:      v.Period,
				},
			}

			err := r.save()
			if err != nil {
				fmt.Println("Here")
				os.Exit(1)
			}

			serverStore[v.Name] = r
		}
	}

	ctxb := context.Background()
	lightningServerStream, err = lightningClient.SubscribeInvoices(ctxb, &lnrpc.InvoiceSubscription{})
	if err != nil {
		log.Fatalf("Lightauth error: Failed to start lightning client stream: %v\n", err)
	}

	go func() {
		for {
			invoiceUpdate, err := lightningServerStream.Recv()
			if err == io.EOF {
				return
			}

			if err != nil {
				log.Printf("Lightauth error: There was an error receiving data from the lightning client stream: %v\n", err)
			}

			if invoiceUpdate != nil && invoiceUpdate.Settled {
				err := updateInvoice(invoiceUpdate.PaymentRequest)
				if err != nil {
					// TODO: Serious error: we have been notified of a payment but we can't save it in database.
				}
			}
		}
	}()

	return conn
}
