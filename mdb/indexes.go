// Copyright 2018 Kuei-chun Chen. All rights reserved.

package mdb

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// IndexesReader holder indexes reader struct
type IndexesReader struct {
	client  *mongo.Client
	dbName  string
	verbose bool
}

// UsageDoc -
type UsageDoc struct {
	hostname string
	Ops      int       `json:"ops" bson:"ops"`
	Snice    time.Time `json:"since" bson:"since"`
}

// IndexStatsDoc -
type IndexStatsDoc struct {
	key          string
	effectiveKey string
	isShardKey   bool
	totalOps     int
	usage        []UsageDoc
}

// NewIndexesReader establish seeding parameters
func NewIndexesReader(client *mongo.Client) *IndexesReader {
	return &IndexesReader{client: client}
}

// SetVerbose sets verbose level
func (ir *IndexesReader) SetVerbose(verbose bool) {
	ir.verbose = verbose
}

// SetDBName sets verbose level
func (ir *IndexesReader) SetDBName(dbName string) {
	ir.dbName = dbName
}

// GetIndexes list all indexes of collections of databases
func (ir *IndexesReader) GetIndexes() (bson.M, error) {
	var err error
	indexesMap := bson.M{}
	if ir.dbName != "" {
		indexesMap[ir.dbName], err = ir.GetIndexesFromDB(ir.dbName)
		return indexesMap, err
	}

	dbNames, _ := ListDatabaseNames(ir.client)
	for _, name := range dbNames {
		if name == "admin" || name == "config" || name == "local" {
			continue
		}
		if indexesMap[name], err = ir.GetIndexesFromDB(name); err != nil {
			return indexesMap, err
		}
	}
	return indexesMap, err
}

// GetIndexesFromDB list all indexes of collections of a database
func (ir *IndexesReader) GetIndexesFromDB(dbName string) (bson.M, error) {
	var err error
	var cur *mongo.Cursor
	var icur *mongo.Cursor
	var scur *mongo.Cursor
	var ctx = context.Background()
	var pipeline = MongoPipeline(`{"$indexStats": {}}`)
	var indexesMap = bson.M{}
	if cur, err = ir.client.Database(dbName).ListCollections(ctx, bson.M{}); err != nil {
		return indexesMap, err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var list []IndexStatsDoc
		var elem = bson.M{}
		if err = cur.Decode(&elem); err != nil {
			continue
		}
		coll := fmt.Sprintf("%v", elem["name"])
		collType := fmt.Sprintf("%v", elem["type"])
		if strings.Index(coll, "system.") == 0 || (elem["type"] != nil && collType != "collection") {
			continue
		}

		if scur, err = ir.client.Database(dbName).Collection(coll).Aggregate(ctx, pipeline); err != nil {
			log.Fatal(err)
		}
		var indexStats = []bson.M{}
		for scur.Next(ctx) {
			var result = bson.M{}
			if err = scur.Decode(&result); err != nil {
				continue
			}
			indexStats = append(indexStats, result)
		}
		scur.Close(ctx)
		indexView := ir.client.Database(dbName).Collection(coll).Indexes()
		if icur, err = indexView.List(ctx); err != nil {
			continue
		}
		defer icur.Close(ctx)

		for icur.Next(ctx) {
			var idx = bson.D{}
			if err = icur.Decode(&idx); err != nil {
				continue
			}

			var keys bson.D
			var indexName string
			for _, v := range idx {
				if v.Key == "name" {
					indexName = v.Value.(string)
				} else if v.Key == "key" {
					keys = v.Value.(bson.D)
				}
			}
			var strbuf bytes.Buffer
			for n, value := range keys {
				if n == 0 {
					strbuf.WriteString("{ ")
				}
				strbuf.WriteString(value.Key + ": " + fmt.Sprint(value.Value))
				if n == len(keys)-1 {
					strbuf.WriteString(" }")
				} else {
					strbuf.WriteString(", ")
				}
			}
			o := IndexStatsDoc{key: strbuf.String()}
			// TODO
			var v bson.M
			if err = ir.client.Database("config").Collection("collections").FindOne(ctx, bson.M{"_id": dbName + "." + coll, "key": keys}).Decode(&v); err == nil {
				o.isShardKey = true
			}
			err = nil
			o.effectiveKey = strings.Replace(o.key[:len(o.key)-2], ": -1", ": 1", -1)
			o.usage = []UsageDoc{}
			for _, result := range indexStats {
				if result["name"].(string) == indexName {
					doc := result["accesses"].(bson.M)
					host := result["host"].(string)
					b, _ := bson.Marshal(doc)
					var accesses UsageDoc
					bson.Unmarshal(b, &accesses)
					accesses.hostname = host
					o.totalOps += accesses.Ops
					o.usage = append(o.usage, accesses)
				}
			}
			list = append(list, o)
		}
		icur.Close(ctx)
		sort.Slice(list, func(i, j int) bool { return (list[i].effectiveKey <= list[j].effectiveKey) })
		indexesMap[coll] = list
	}
	return indexesMap, err
}

// Print prints indexes
func (ir *IndexesReader) Print(indexesMap bson.M) {
	for _, key := range getSortedKeys(indexesMap) {
		val := indexesMap[key].(bson.M)
		for _, k := range getSortedKeys(val) {
			list := val[k].([]IndexStatsDoc)
			var buffer bytes.Buffer
			ns := key + "." + k
			buffer.WriteString("\n")
			buffer.WriteString(ns)
			buffer.WriteString(":\n")
			for i, o := range list {
				font := "\x1b[0m  "
				if o.key != "{ _id: 1 }" && o.isShardKey == false {
					if i < len(list)-1 && strings.Index(list[i+1].effectiveKey, o.effectiveKey) == 0 {
						font = "\x1b[31;1mx " // red
					} else {
						if o.totalOps == 0 {
							font = "\x1b[34;1m? " // blue
						}
					}
				} else if o.isShardKey == true {
					font = "\x1b[0m* "
				}
				buffer.WriteString(font + o.key + "\x1b[0m")
				for _, u := range o.usage {
					buffer.Write([]byte("\n\thost: " + u.hostname + ", ops: " + fmt.Sprintf("%v", u.Ops) + ", since: " + fmt.Sprintf("%v", u.Snice)))
				}
				buffer.WriteString("\n")
			}
			fmt.Println(buffer.String())
		}
	}
}

func getSortedKeys(rmap bson.M) []string {
	var keys []string
	for k := range rmap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
