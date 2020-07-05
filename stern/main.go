//   Copyright 2020
//
//   Licensed under the Apache License, Version 2.0 (the "License");
//   you may not use this file except in compliance with the License.
//   You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
//   Unless required by applicable law or agreed to in writing, software
//   distributed under the License is distributed on an "AS IS" BASIS,
//   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//   See the License for the specific language governing permissions and
//   limitations under the License.

package stern

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"time"

	"github.com/pkg/errors"
	"github.com/wercker/stern/kubernetes"
	"gopkg.in/Graylog2/go-gelf.v2/gelf"
)

// Run starts the main run loop
func Run(ctx context.Context, config *Config) error {
	rand.Seed(time.Now().UnixNano())

	clientConfig := kubernetes.NewClientConfig(config.KubeConfig, config.ContextName)
	clientset, err := kubernetes.NewClientSet(clientConfig)
	if err != nil {
		return err
	}

	var writerErr error
	var gelfWriter *gelf.TCPWriter
	var sleep time.Duration = time.Second * 10

	if config.GraylogServer != "" {
		for {
			gelfWriter, writerErr = gelf.NewTCPWriter(config.GraylogServer)
			if writerErr != nil {
				if config.GraylogRetries--; config.GraylogRetries > 0 {
					// Add some randomness to prevent creating a Thundering Herd
					jitter := time.Duration(rand.Int63n(int64(sleep)))
					sleep = (sleep + jitter/2)
					timeNow := time.Now().Format("2006/01/02 15:04:05")
					os.Stderr.WriteString(fmt.Sprintf(timeNow+" Could not connect to Graylog Server, next retry in %s. "+strconv.Itoa(config.GraylogRetries)+" retries left. \n", sleep.Round(time.Second)))
					time.Sleep(sleep)
					gelfWriter = nil
					writerErr = nil
					continue
				} else {
					return errors.Wrap(writerErr, "setup gelf writer failed")
				}
			} else {
				break
			}
		}
		gelfWriter.MaxReconnect = 40
		gelfWriter.ReconnectDelay = 15
		//	return errors.Wrap(err, "Graylog Server address unset")
	}

	var namespace string
	// A specific namespace is ignored if all-namespaces is provided
	if config.AllNamespaces {
		namespace = ""
	} else {
		namespace = config.Namespace
		if namespace == "" {
			namespace, _, err = clientConfig.Namespace()
			if err != nil {
				return errors.Wrap(err, "unable to get default namespace")
			}
		}
	}

	added, removed, err := Watch(ctx, clientset.CoreV1().Pods(namespace), config.PodQuery, config.ContainerQuery, config.ExcludeContainerQuery, config.ContainerState, config.LabelSelector)
	if err != nil {
		return errors.Wrap(err, "failed to set up watch")
	}

	tails := make(map[string]*Tail)
	logC := make(chan string, 1024)

	go func() {
		for {
			select {
			case str := <-logC:
				fmt.Fprintf(os.Stdout, str)
			case <-ctx.Done():
				break
			}
		}
	}()

	go func() {
		for p := range added {
			id := p.GetID()
			if tails[id] != nil {
				continue
			}

			tail := NewTail(p.Namespace, p.Pod, p.Container, config.Template, &TailOptions{
				Timestamps:    config.Timestamps,
				SinceSeconds:  int64(config.Since.Seconds()),
				Exclude:       config.Exclude,
				Include:       config.Include,
				Namespace:     config.AllNamespaces,
				TailLines:     config.TailLines,
				ContextName:   config.ContextName,
				GraylogServer: config.GraylogServer,
			})
			tails[id] = tail

			tail.Start(ctx, clientset.CoreV1().Pods(p.Namespace), gelfWriter, logC)
		}
	}()

	go func() {
		for p := range removed {
			id := p.GetID()
			if tails[id] == nil {
				continue
			}
			tails[id].Close()
			delete(tails, id)
		}
	}()

	<-ctx.Done()

	return nil
}
