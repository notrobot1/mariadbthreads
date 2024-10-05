package mariadbthreads

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"

	"log"

	ufsio "github.com/ipfs/boxo/ipld/unixfs/io"
	ipns "github.com/ipfs/boxo/ipns"
	"github.com/ipfs/boxo/path"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	_ "github.com/mattn/go-sqlite3"
)

// To open File
// Add the file to ipfs and public ipns
// Notifi about the new record
func PublicDB(ctx context.Context, dbName string, p *Peer, privKey crypto.PrivKey) error {

	file, err := os.Open(dbName)
	if err != nil {
		return err
	}
	defer file.Close()

	//Add file to ipfs
	fileNode, err := p.AddFile(ctx, file, nil)
	if err != nil {
		return err
	}
	//Create path
	value, err := path.NewPath("/ipfs/" + fileNode.String())
	if err != nil {
		return err
	}

	//Publick DB in IPNS
	pid, _ := peer.IDFromPublicKey(privKey.GetPublic())
	ipnsPath, err := PublicFile(ctx, privKey, value, p, pid)
	if err != nil {
		return err
	}
	fmt.Println(ipnsPath.String())
	
	return nil
}

func GetDB(p *Peer, ctx context.Context, name ipns.Name) error {

	pathFile, err := GetFileHash(p, name)
	if err != nil {
		return err
	}

	log.Println(pathFile)
	c, err := cid.Decode(pathFile.Segments()[1])
	if err != nil {
		return err
	}
	n, err := p.Get(ctx, c)
	if err != nil {
		return err
	}

	rsc, err := ufsio.NewDagReader(ctx, n, p)
	if err != nil {
		return err
	}
	defer rsc.Close()
	content, err := io.ReadAll(rsc)
	if err != nil {
		return err
	}

	file, err := os.Create(name.String())
	if err != nil {
		return err
	}
	defer file.Close()

	// Запись данных в файл
	_, err = file.Write(content)
	return err
}

func connectDB(dbName string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbName)
	if err != nil {
		return nil, err
	}

	return db, nil
}

// sqlStmt := `
// 	CREATE TABLE IF NOT EXISTS users (
// 		id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
// 		name TEXT,
// 		age INTEGER
// 	);
// 	`

func CreateTable(sqlStmt, dbname string) error {
	db, err := connectDB(dbname)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(sqlStmt)
	if err != nil {
		return err
	}
	return nil
}

func Insert(dbname, insert string, values ...interface{}) error {
	db, err := connectDB(dbname)
	if err != nil {
		return err
	}
	defer db.Close()

	// Вставка данных
	_, err = db.Exec(insert, values...)
	if err != nil {
		return err
	}
	//Public file

	return nil
}

func Select(dbname, selectQ string, values ...interface{}) (*sql.Rows, error) {
	db, err := connectDB(dbname)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	fmt.Println(values...)
	rows, err := db.Query(selectQ, values...)
	if err != nil {
		return nil, err
	}
	return rows, nil

}
