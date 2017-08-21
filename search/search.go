package search

import (
	"bytes"
	"container/list"
	"context"
	"crypto/md5"
	"github.com/Sirupsen/logrus"
	"github.com/dropbox/godropbox/errors"
	"github.com/pritunl/pritunl-zero/errortypes"
	"github.com/pritunl/pritunl-zero/requires"
	"github.com/pritunl/pritunl-zero/settings"
	"gopkg.in/mgo.v2/bson"
	"gopkg.in/olivere/elastic.v5"
	"io"
	"sync"
	"time"
)

var (
	ctx          = context.Background()
	client       *elastic.Client
	buffer       = list.New()
	failedBuffer = []*elastic.BulkIndexRequest{}
	lock         = sync.Mutex{}
	failedLock   = sync.Mutex{}
)

type mapping struct {
	Field string
	Type  string
	Store bool
	Index string
}

func Index(index, typ string, data interface{}) {
	clnt := client
	if clnt == nil {
		return
	}

	id := bson.NewObjectId().Hex()

	request := elastic.NewBulkIndexRequest().Index(index).Type(typ).
		Id(id).Doc(data)

	lock.Lock()
	buffer.PushBack(request)
	lock.Unlock()

	return
}

func putIndex(clnt *elastic.Client, index string, typ string,
	mappings []mapping) (err error) {

	exists, err := clnt.IndexExists(index).Do(ctx)
	if err != nil {
		err = errortypes.DatabaseError{
			errors.Wrap(err, "search: Failed to check elastic index"),
		}
		return
	}

	if exists {
		return
	}

	properties := map[string]interface{}{}

	for _, mapping := range mappings {
		if mapping.Type == "object" {
			properties[mapping.Field] = struct {
				Enabled bool `json:"enabled"`
			}{
				Enabled: false,
			}
		} else {
			properties[mapping.Field] = struct {
				Type  string `json:"type"`
				Store bool   `json:"store"`
				Index string `json:"index"`
			}{
				Type:  mapping.Type,
				Store: mapping.Store,
				Index: mapping.Index,
			}
		}
	}

	data := struct {
		Mappings map[string]interface{} `json:"mappings"`
	}{
		Mappings: map[string]interface{}{},
	}

	data.Mappings[typ] = struct {
		Properties map[string]interface{} `json:"properties"`
	}{
		Properties: properties,
	}

	_, err = clnt.CreateIndex(index).BodyJson(data).Do(ctx)
	if err != nil {
		err = errortypes.DatabaseError{
			errors.Wrap(err, "search: Failed to create elastic index"),
		}
		return
	}

	return
}

func newClient(addrs []string) (clnt *elastic.Client, err error) {
	if len(addrs) == 0 {
		return
	}

	clnt, err = elastic.NewClient(
		elastic.SetSniff(false),
		elastic.SetURL(addrs...),
	)
	if err != nil {
		err = errortypes.DatabaseError{
			errors.Wrap(err, "search: Failed to create elastic client"),
		}
		return
	}

	return
}

func hashAddresses(addrs []string) []byte {
	hash := md5.New()

	for _, addr := range addrs {
		io.WriteString(hash, addr)
	}

	return hash.Sum(nil)
}

func update(addrs []string) (err error) {
	clnt, err := newClient(addrs)
	if err != nil {
		client = nil
		return
	}

	if clnt == nil {
		client = nil
		return
	}

	mappings := []mapping{}

	mappings = append(mappings, mapping{
		Field: "user",
		Type:  "keyword",
		Store: false,
		Index: "analyzed",
	})

	mappings = append(mappings, mapping{
		Field: "session",
		Type:  "keyword",
		Store: false,
		Index: "analyzed",
	})

	mappings = append(mappings, mapping{
		Field: "address",
		Type:  "ip",
		Store: false,
		Index: "analyzed",
	})

	mappings = append(mappings, mapping{
		Field: "timestamp",
		Type:  "date",
		Store: false,
		Index: "analyzed",
	})

	mappings = append(mappings, mapping{
		Field: "scheme",
		Type:  "keyword",
		Store: false,
		Index: "analyzed",
	})

	mappings = append(mappings, mapping{
		Field: "host",
		Type:  "keyword",
		Store: false,
		Index: "analyzed",
	})

	mappings = append(mappings, mapping{
		Field: "path",
		Type:  "keyword",
		Store: false,
		Index: "analyzed",
	})

	mappings = append(mappings, mapping{
		Field: "query",
		Type:  "object",
	})

	mappings = append(mappings, mapping{
		Field: "header",
		Type:  "object",
	})

	mappings = append(mappings, mapping{
		Field: "body",
		Type:  "text",
		Store: false,
		Index: "no",
	})

	err = putIndex(clnt, "zero-requests", "request", mappings)
	if err != nil {
		client = nil
		return
	}

	client = clnt

	return
}

func watchSearch() {
	hash := hashAddresses([]string{})

	for {
		addrs := settings.Elastic.Addresses
		newHash := hashAddresses(addrs)

		if bytes.Compare(hash, newHash) != 0 {
			err := update(addrs)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"error": err,
				}).Error("search: Failed to update search indexes")
				time.Sleep(3 * time.Second)
				continue
			}

			hash = newHash
		}

		time.Sleep(1 * time.Second)
	}
}

func worker() {
	for {
		time.Sleep(2 * time.Second)

		requests := []*elastic.BulkIndexRequest{}

		lock.Lock()
		for elem := buffer.Front(); elem != nil; elem = elem.Next() {
			request := elem.Value.(*elastic.BulkIndexRequest)
			requests = append(requests, request)
		}
		buffer = list.New()
		lock.Unlock()

		clnt := client
		if client == nil {
			continue
		}

		if len(requests) == 0 {
			continue
		}

		var err error
		for i := 0; i < 10; i++ {
			bulk := clnt.Bulk()

			for _, request := range requests {
				bulk.Add(request)
			}

			_, err = bulk.Do(ctx)
			if err != nil {
				time.Sleep(1 * time.Second)
				continue
			}

			err = nil
			break
		}

		if err != nil {
			failedLock.Lock()
			failedBuffer = append(failedBuffer, requests...)
			failedLock.Unlock()

			logrus.WithFields(logrus.Fields{
				"error": err,
			}).Error("search: Bulk insert failed, moving to failed buffer")

			err = nil
		}
	}
}

func init() {
	module := requires.New("search")
	module.After("settings")

	module.Handler = func() (err error) {
		go watchSearch()
		go worker()
		return
	}
}