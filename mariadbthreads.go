package mariadbthreads

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ipfs/boxo/bitswap"
	"github.com/ipfs/boxo/bitswap/network"
	"github.com/ipfs/boxo/blockservice"
	chunker "github.com/ipfs/boxo/chunker"
	"github.com/ipfs/boxo/ipld/merkledag"
	ipns "github.com/ipfs/boxo/ipns"
	"github.com/ipfs/boxo/namesys"
	"github.com/ipfs/boxo/path"
	provider "github.com/ipfs/boxo/provider"
	datastore "github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	exchange "github.com/ipfs/go-ipfs-exchange-interface"
	offline "github.com/ipfs/go-ipfs-exchange-offline"
	ipld "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/go-unixfs/importer/balanced"
	"github.com/ipfs/go-unixfs/importer/helpers"
	"github.com/ipfs/go-unixfs/importer/trickle"

	//"github.com/ipld/go-ipld-prime"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"
	"github.com/multiformats/go-multihash"
)

var (
	defaultReprovideInterval = 12 * time.Hour
)

func NewInMemoryDatastore() datastore.Batching {
	return dssync.MutexWrap(datastore.NewMapDatastore())
}

func (cfg *Config) setDefaults() {
	if cfg.ReprovideInterval == 0 {
		cfg.ReprovideInterval = defaultReprovideInterval
	}
}

func (p *Peer) autoclose() {
	<-p.ctx.Done()
	p.reprovider.Close()
	p.bserv.Close()
}

func (p *Peer) setupDAGService() error {
	p.DAGService = merkledag.NewDAGService(p.bserv)
	return nil
}

func (p *Peer) setupBlockstore(bs blockstore.Blockstore) error {
	var err error
	if bs == nil {
		bs = blockstore.NewBlockstore(p.store)
	}

	// Support Identity multihashes.
	bs = blockstore.NewIdStore(bs)

	if !p.cfg.UncachedBlockstore {
		bs, err = blockstore.CachedBlockstore(p.ctx, bs, blockstore.DefaultCacheOpts())
		if err != nil {
			return err
		}
	}
	p.bstore = bs
	return nil
}

func (p *Peer) setupBlockService() error {
	if p.cfg.Offline {
		p.bserv = blockservice.New(p.bstore, offline.Exchange(p.bstore))
		return nil
	}

	bswapnet := network.NewFromIpfsHost(p.host, p.dht)
	bswap := bitswap.New(p.ctx, bswapnet, p.bstore)
	p.bserv = blockservice.New(p.bstore, bswap)
	p.exch = bswap
	return nil
}

func (p *Peer) setupReprovider() error {
	if p.cfg.Offline || p.cfg.ReprovideInterval < 0 {
		p.reprovider = provider.NewNoopProvider()
		return nil
	}

	prov, err := provider.New(p.store,
		provider.DatastorePrefix(datastore.NewKey("repro")),
		provider.Online(p.dht),
		provider.ReproviderInterval(p.cfg.ReprovideInterval),
		provider.KeyProvider(provider.NewBlockstoreProvider(p.bstore)))
	if err != nil {
		return err
	}
	p.reprovider = prov

	return nil
}

// Config wraps configuration options for the Peer.
type Config struct {
	// The DAGService will not announce or retrieve blocks from the network
	Offline bool
	// ReprovideInterval sets how often to reprovide records to the DHT
	ReprovideInterval time.Duration
	// Disables wrapping the blockstore in an ARC cache + Bloomfilter. Use
	// when the given blockstore or datastore already has caching, or when
	// caching is not needed.
	UncachedBlockstore bool
}

type Peer struct {
	ctx context.Context

	cfg *Config

	host  host.Host
	dht   routing.Routing
	store datastore.Batching

	ipld.DAGService // become a DAG service
	exch            exchange.Interface
	bstore          blockstore.Blockstore
	bserv           blockservice.BlockService
	reprovider      provider.System
}

func New(
	ctx context.Context,
	datastore datastore.Batching,
	blockstore blockstore.Blockstore,
	host host.Host,
	dht routing.Routing,
	cfg *Config,
) (*Peer, error) {

	if cfg == nil {
		cfg = &Config{}
	}

	cfg.setDefaults()

	p := &Peer{
		ctx:   ctx,
		cfg:   cfg,
		host:  host,
		dht:   dht,
		store: datastore,
	}

	err := p.setupBlockstore(blockstore)
	if err != nil {
		return nil, err
	}
	err = p.setupBlockService()
	if err != nil {
		return nil, err
	}
	err = p.setupDAGService()
	if err != nil {
		p.bserv.Close()
		return nil, err
	}
	err = p.setupReprovider()
	if err != nil {
		p.bserv.Close()
		return nil, err
	}

	go p.autoclose()

	return p, nil
}

func ConnectToDefaultPeers(ctx context.Context, p *Peer) {
	bootstrapPeers := dht.DefaultBootstrapPeers
	for _, addrInfo := range bootstrapPeers {
		peerinfo, _ := peer.AddrInfoFromP2pAddr(addrInfo)
		if err := p.host.Connect(ctx, *peerinfo); err != nil {
			fmt.Println("Failed to connect to bootstrap peer:", err)
		} else {
			fmt.Println(fmt.Sprintf("Conected to %s", peerinfo.ID))
		}
	}
}

// AddParams contains all of the configurable parameters needed to specify the
// importing process of a file.
type AddParams struct {
	Layout    string
	Chunker   string
	RawLeaves bool
	Hidden    bool
	Shard     bool
	NoCopy    bool
	HashFun   string
}

func (p *Peer) AddFile(ctx context.Context, r io.Reader, params *AddParams) (ipld.Node, error) {
	if params == nil {
		params = &AddParams{}
	}
	if params.HashFun == "" {
		params.HashFun = "sha2-256"
	}

	prefix, err := merkledag.PrefixForCidVersion(1)
	if err != nil {
		return nil, fmt.Errorf("bad CID Version: %s", err)
	}

	hashFunCode, ok := multihash.Names[strings.ToLower(params.HashFun)]
	if !ok {
		return nil, fmt.Errorf("unrecognized hash function: %s", params.HashFun)
	}
	prefix.MhType = hashFunCode
	prefix.MhLength = -1

	dbp := helpers.DagBuilderParams{
		Dagserv:    p,
		RawLeaves:  params.RawLeaves,
		Maxlinks:   helpers.DefaultLinksPerBlock,
		NoCopy:     params.NoCopy,
		CidBuilder: &prefix,
	}

	chnk, err := chunker.FromString(r, params.Chunker)
	if err != nil {
		return nil, err
	}
	dbh, err := dbp.New(chnk)
	if err != nil {
		return nil, err
	}

	var n ipld.Node
	switch params.Layout {
	case "trickle":
		n, err = trickle.Layout(dbh)
	case "balanced", "":
		n, err = balanced.Layout(dbh)
	default:
		return nil, errors.New("invalid Layout")
	}
	return n, err
}

func saveKeyToFile() (crypto.PrivKey, crypto.PubKey) {
	fmt.Println("[+] Generate keys")
	privKey, pubKey, err := crypto.GenerateKeyPairWithReader(crypto.RSA, 2048, rand.Reader)
	//priv, _, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
	if err != nil {
		panic(err)
	}
	privKeyByte, _ := crypto.MarshalPrivateKey(privKey)
	pubKeyByte, _ := crypto.MarshalPublicKey(pubKey)
	err = os.WriteFile("key.priv", privKeyByte, 0644)
	if err != nil {

	}
	err = os.WriteFile("key.pub", pubKeyByte, 0644)
	if err != nil {
		panic(err)
	}
	return privKey, pubKey
}

// The function is called to load keys. If there are no keys, they will be created and saved.
func LoadKeyFromFile() (crypto.PrivKey, crypto.PubKey) {
	privKey, err := os.ReadFile("key.priv")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			privKey, pubKey := saveKeyToFile()
			return privKey, pubKey
		} else {
			panic(err)
		}
	}

	pubKey, err := os.ReadFile("key.pub")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			privKey, pubKey := saveKeyToFile()
			return privKey, pubKey
		} else {
			panic(err)
		}
	}
	privKeyByte, err := crypto.UnmarshalPrivateKey(privKey)
	if err != nil {
		panic(err)
	}
	pubKeyByte, err := crypto.UnmarshalPublicKey(pubKey)
	if err != nil {
		panic(err)
	}
	return privKeyByte, pubKeyByte
}

// The function publishes a file to the IPFS network and updates the IPNS record
func PublicFile(ctx context.Context, privKey crypto.PrivKey, value path.Path, p *Peer, pid peer.ID) (path.Path, error) {
	rec, err := ipns.NewRecord(privKey, value, 0, time.Now().Add(24*time.Hour), 0)
	if err != nil {
		return nil, err
	}

	err = namesys.PublishIPNSRecord(ctx, p.dht, privKey.GetPublic(), rec)
	if err != nil {
		return nil, err
	}

	return ipns.NameFromPeer(pid).AsPath(), nil
}

// The function returns the file Path by IPNS record
func GetFileHash(p *Peer, name ipns.Name) (path.Path, error) {
	resolver := namesys.NewIPNSResolver(p.dht)
	res, err := resolver.Resolve(context.Background(), name.AsPath())
	if err != nil {
		return nil, err
	}
	return res.Path, nil
}

// The function gets all known IPNS records and downloads the database
func UpdateAllDataBase(p *Peer, ctx context.Context) error {
	file, err := os.Open(".members")
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Println(line)
		//If there is no error, then the IPNS name has arrived
		data, err := ipns.NameFromString(string(line))
		if err == nil {
			err := GetDB(p, ctx, data)
			if err != nil {
				fmt.Println(err.Error())
			}
		}
	}

	return nil
}

// func FindPeers(ctx context.Context) {

// 	provChan := dht.FindProviders()
// 	fmt.Println("Searching for providers...")

// 	for p := range provChan {
// 		fmt.Printf("Found provider: %s\n", p.ID.Pretty())
// 	}
// }
