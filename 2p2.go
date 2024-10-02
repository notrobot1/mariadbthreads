package mariadbthreads

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	ipns "github.com/ipfs/boxo/ipns"
	"github.com/ipfs/go-datastore"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	dualdht "github.com/libp2p/go-libp2p-kad-dht/dual"
	pubsub "github.com/libp2p/go-libp2p-pubsub"

	//pubsub "github.com/libp2p/go-libp2p-pubsub"
	record "github.com/libp2p/go-libp2p-record"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/pnet"
	"github.com/libp2p/go-libp2p/core/routing"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/libp2p/go-libp2p/p2p/transport/websocket"
	"github.com/multiformats/go-multiaddr"
)

var connMgr, _ = connmgr.NewConnManager(100, 600, connmgr.WithGracePeriod(time.Minute))
var Libp2pOptionsExtra = []libp2p.Option{
	libp2p.NATPortMap(),
	libp2p.ConnectionManager(connMgr),
	//libp2p.EnableAutoRelay(),
	libp2p.EnableNATService(),
}

// Configuring libp2p
func SetupLibp2p(ctx context.Context,
	hostKey crypto.PrivKey,
	secret pnet.PSK,
	listenAddrs []multiaddr.Multiaddr,
	ds datastore.Batching,
	opts ...libp2p.Option) (host.Host, *dualdht.DHT, peer.ID, error) {
	var ddht *dualdht.DHT

	var err error
	var transports = libp2p.DefaultTransports

	if secret != nil {
		transports = libp2p.ChainOptions(
			libp2p.NoTransports,
			libp2p.Transport(tcp.NewTCPTransport),
			libp2p.Transport(websocket.New),
		)
	}

	finalOpts := []libp2p.Option{
		libp2p.Identity(hostKey),
		libp2p.ListenAddrs(listenAddrs...),
		libp2p.PrivateNetwork(secret),
		transports,
		libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
			ddht, err = newDHT(ctx, h, ds)
			return ddht, err
		}),
	}
	finalOpts = append(finalOpts, opts...)

	h, err := libp2p.New(
		finalOpts...,
	)
	if err != nil {
		return nil, nil, "", err
	}

	pid, _ := peer.IDFromPublicKey(hostKey.GetPublic())
	//Connect to default pears
	defaultPeers(ctx, h)
	return h, ddht, pid, nil
}

// Initialize DHT
func newDHT(ctx context.Context, h host.Host, ds datastore.Batching) (*dualdht.DHT, error) {
	dhtOpts := []dualdht.Option{
		dualdht.DHTOption(dht.NamespacedValidator("pk", record.PublicKeyValidator{})),
		dualdht.DHTOption(dht.NamespacedValidator("ipns", ipns.Validator{KeyBook: h.Peerstore()})),
		dualdht.DHTOption(dht.Concurrency(10)),
		dualdht.DHTOption(dht.Mode(dht.ModeAuto)),
	}
	if ds != nil {
		dhtOpts = append(dhtOpts, dualdht.DHTOption(dht.Datastore(ds)))
	}

	return dualdht.New(ctx, h, dhtOpts...)

}

// The function connects to the default Bootstrap peers
func defaultPeers(ctx context.Context, h host.Host) {
	bootstrapPeers := dht.DefaultBootstrapPeers
	bootstrapPeers = bootstrapPeers[:len(bootstrapPeers)-1]
	fmt.Println(len(bootstrapPeers))
	for _, addrInfo := range bootstrapPeers {
		peerinfo, _ := peer.AddrInfoFromP2pAddr(addrInfo)
		if err := h.Connect(ctx, *peerinfo); err != nil {
			fmt.Println("Failed to connect to bootstrap peer:", err)
		} else {
			fmt.Println(peerinfo)
		}
		fmt.Println(peerinfo.Addrs)
	}
}

func Subscribe(ctx context.Context, h host.Host, topic string) (*pubsub.Subscription, *pubsub.PubSub) {
	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		log.Fatalln(err)
	}

	// Подписка на тему
	sub, err := ps.Subscribe(topic)
	if err != nil {
		log.Fatalln(err)
	}

	// Запуск горутины для обработки сообщений
	go func() {
		for {
			msg, err := sub.Next(ctx)
			if err != nil {
				fmt.Println(err.Error())
			}
			//We don't receive messages from ourselves
			if msg.ReceivedFrom.String() != h.ID().String() {
				if string(msg.Data) == "Hello" {
					//TODO Notify the new client of all IPNS records that we know
					fmt.Println("Its Hello")
				} else {
					//If there is no error, then the IPNS name has arrived
					data, err := ipns.NameFromString(string(msg.Data))
					if err == nil {
						//Saving a new member
						file, _ := os.OpenFile(".members", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
						//defer file.Close()

						// Записываем данные в файл
						if _, err := file.WriteString(data.String()); err != nil {
							fmt.Println("Ошибка при записи в файл:", err)
						} else {
							fmt.Println("Данные успешно записаны")
						}
						fmt.Printf("Received message: %s from %s me %s\n", string(msg.Data), msg.ReceivedFrom, h.ID().String())
					}
				}

			} else {
				fmt.Println("ITS ME")
			}
		}

	}()

	// Публикация сообщения в тему
	err = ps.Publish(topic, []byte("Hello"))
	if err != nil {
		log.Fatalln(err)
	}
	return sub, ps

}
