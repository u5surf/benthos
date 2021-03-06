// Copyright (c) 2018 Ashley Jeffs
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package writer

import (
	"errors"
	"fmt"
	"time"

	"github.com/Jeffail/benthos/lib/log"
	"github.com/Jeffail/benthos/lib/message"
	"github.com/Jeffail/benthos/lib/metrics"
	"github.com/Jeffail/benthos/lib/types"
	"github.com/Jeffail/benthos/lib/util/aws/session"
	"github.com/Jeffail/benthos/lib/util/retries"
	"github.com/Jeffail/benthos/lib/util/text"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbiface"
	"github.com/cenkalti/backoff"
)

//------------------------------------------------------------------------------

// DynamoDBConfig contains config fields for the DynamoDB output type.
type DynamoDBConfig struct {
	sessionConfig  `json:",inline" yaml:",inline"`
	Table          string            `json:"table" yaml:"table"`
	StringColumns  map[string]string `json:"string_columns" yaml:"string_columns"`
	TTL            string            `json:"ttl" yaml:"ttl"`
	TTLKey         string            `json:"ttl_key" yaml:"ttl_key"`
	retries.Config `json:",inline" yaml:",inline"`
}

// NewDynamoDBConfig creates a DynamoDBConfig populated with default values.
func NewDynamoDBConfig() DynamoDBConfig {
	rConf := retries.NewConfig()
	rConf.MaxRetries = 3
	rConf.Backoff.InitialInterval = "1s"
	rConf.Backoff.MaxInterval = "5s"
	rConf.Backoff.MaxElapsedTime = "30s"
	return DynamoDBConfig{
		sessionConfig: sessionConfig{
			Config: session.NewConfig(),
		},
		Table:         "",
		StringColumns: map[string]string{},
		TTL:           "",
		TTLKey:        "",
		Config:        rConf,
	}
}

//------------------------------------------------------------------------------

// DynamoDB is a benthos writer.Type implementation that writes messages to an
// Amazon SQS queue.
type DynamoDB struct {
	client  dynamodbiface.DynamoDBAPI
	conf    DynamoDBConfig
	log     log.Modular
	stats   metrics.Type
	backoff backoff.BackOff

	table      *string
	ttl        time.Duration
	strColumns map[string]*text.InterpolatedString
}

// NewDynamoDB creates a new Amazon SQS writer.Type.
func NewDynamoDB(
	conf DynamoDBConfig,
	log log.Modular,
	stats metrics.Type,
) (*DynamoDB, error) {
	boff, err := conf.Get()
	if err != nil {
		return nil, fmt.Errorf("failed to parse retry fields: %v", err)
	}
	db := &DynamoDB{
		conf:       conf,
		log:        log.NewModule(".output.dynamodb"),
		stats:      stats,
		table:      aws.String(conf.Table),
		backoff:    boff,
		strColumns: map[string]*text.InterpolatedString{},
	}
	if len(conf.StringColumns) == 0 {
		return nil, errors.New("you must provide at least one column")
	}
	for k, v := range conf.StringColumns {
		db.strColumns[k] = text.NewInterpolatedString(v)
	}
	if conf.TTL != "" {
		ttl, err := time.ParseDuration(conf.TTL)
		if err != nil {
			return nil, fmt.Errorf("failed to parse TTL: %v", err)
		}
		db.ttl = ttl
	}
	return db, nil
}

// Connect attempts to establish a connection to the target SQS queue.
func (d *DynamoDB) Connect() error {
	if d.client != nil {
		return nil
	}

	sess, err := d.conf.GetSession()
	if err != nil {
		return err
	}

	client := dynamodb.New(sess)
	out, err := client.DescribeTable(&dynamodb.DescribeTableInput{
		TableName: d.table,
	})
	if err != nil {
		return err
	} else if out == nil || out.Table == nil || out.Table.TableStatus == nil || *out.Table.TableStatus != dynamodb.TableStatusActive {
		return fmt.Errorf("dynamodb table '%s' must be active", d.conf.Table)
	}

	d.client = client
	d.log.Infof("Sending messages to DynamoDB table: %v\n", d.conf.Table)
	return nil
}

// Write attempts to write message contents to a target SQS.
func (d *DynamoDB) Write(msg types.Message) error {
	if d.client == nil {
		return types.ErrNotConnected
	}

	writeReqs := []*dynamodb.WriteRequest{}
	msg.Iter(func(i int, p types.Part) error {
		items := map[string]*dynamodb.AttributeValue{}
		if d.ttl != 0 && d.conf.TTLKey != "" {
			items[d.conf.TTLKey] = &dynamodb.AttributeValue{
				S: aws.String(time.Now().Add(d.ttl).Format(time.RFC3339Nano)),
			}
		}
		for k, v := range d.strColumns {
			s := v.Get(message.Lock(msg, i))
			items[k] = &dynamodb.AttributeValue{
				S: &s,
			}
		}
		writeReqs = append(writeReqs, &dynamodb.WriteRequest{
			PutRequest: &dynamodb.PutRequest{
				Item: items,
			},
		})
		return nil
	})

	var err error
	for len(writeReqs) > 0 {
		wait := d.backoff.NextBackOff()
		var batchResult *dynamodb.BatchWriteItemOutput
		batchResult, err = d.client.BatchWriteItem(&dynamodb.BatchWriteItemInput{
			RequestItems: map[string][]*dynamodb.WriteRequest{
				*d.table: writeReqs,
			},
		})
		if err != nil {
			d.log.Errorf("Write multi error: %v\n", err)
		} else if unproc := batchResult.UnprocessedItems[*d.table]; len(unproc) > 0 {
			writeReqs = unproc
			err = fmt.Errorf("failed to set %v items", len(unproc))
		} else {
			writeReqs = nil
		}

		if err != nil {
			if wait == backoff.Stop {
				break
			}
			time.After(wait)
		}
	}

	if err == nil {
		d.backoff.Reset()
	}
	return nil
}

// CloseAsync begins cleaning up resources used by this writer asynchronously.
func (d *DynamoDB) CloseAsync() {
}

// WaitForClose will block until either the writer is closed or a specified
// timeout occurs.
func (d *DynamoDB) WaitForClose(time.Duration) error {
	return nil
}

//------------------------------------------------------------------------------
