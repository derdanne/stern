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
	"strings"
	"sync"
	"time"

	"github.com/derdanne/stern/kubernetes"
	"github.com/pkg/errors"
	"gopkg.in/Graylog2/go-gelf.v2/gelf"
)

// Run starts the main run loop
func Run(ctx context.Context, config *Config) error {
	rand.Seed(time.Now().UnixNano())
	clientTimeoutSeconds := int64(config.ClientTimeout)
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
		gelfWriter.MaxReconnect = 30
		gelfWriter.ReconnectDelay = 5
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

	var w sync.WaitGroup
	var added chan *Target
	var removed chan *Target
	closed := make(chan bool)
	restart := make(chan bool)
	namespaces := strings.Split(namespace, ",")
	watcherCount := len(namespaces)

OUTER:
	for {
		for _, namespace := range namespaces {
			added, removed, err = Watch(ctx,
				clientset.CoreV1().Pods(namespace),
				config.PodQuery,
				config.ContainerQuery,
				config.ExcludeContainerQuery,
				config.InitContainers,
				config.ContainerState,
				config.LabelSelector,
				clientTimeoutSeconds,
				closed,
				restart)
			if err != nil {
				return errors.Wrap(err, "failed to set up watch")
			}

			tails := make(map[string]*Tail)
			tailsMutex := sync.RWMutex{}
			logC := make(chan string, 1024)

			go func() {
				defer w.Done()
				w.Add(1)
				select {
				case <-closed:
					for id := range tails {
						tailsMutex.RLock()
						existing := tails[id]
						tailsMutex.RUnlock()
						if existing == nil {
							continue
						}
						tailsMutex.Lock()
						tails[id].Close()
						delete(tails, id)
						tailsMutex.Unlock()
					}
					logC = nil
				case <-ctx.Done():
					break
				}
			}()

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
				defer w.Done()
				w.Add(1)
				for p := range added {
					id := p.GetID()
					tailsMutex.RLock()
					existing := tails[id]
					tailsMutex.RUnlock()
					if existing != nil {
						if existing.Active == true {
							continue
						} else { // cleanup failed tail to restart
							tailsMutex.Lock()
							tails[id].Close()
							delete(tails, id)
							tailsMutex.Unlock()
						}
					}

					tail := NewTail(p.Namespace, p.Pod, p.Container, p.NodeName, config.Template, &TailOptions{
						Timestamps:    config.Timestamps,
						SinceSeconds:  int64(config.Since.Seconds()),
						Exclude:       config.Exclude,
						Include:       config.Include,
						Namespace:     config.AllNamespaces,
						TailLines:     config.TailLines,
						ContextName:   config.ContextName,
						ClusterName:   config.ClusterName,
						GraylogServer: config.GraylogServer,
					})
					tailsMutex.Lock()
					tails[id] = tail
					tailsMutex.Unlock()
					tail.Start(ctx, clientset.CoreV1().Pods(p.Namespace), gelfWriter, logC)
				}
			}()

			go func() {
				defer w.Done()
				w.Add(1)
				for p := range removed {
					id := p.GetID()
					tailsMutex.RLock()
					existing := tails[id]
					tailsMutex.RUnlock()
					if existing == nil {
						continue
					}
					tailsMutex.Lock()
					tails[id].Close()
					delete(tails, id)
					tailsMutex.Unlock()
				}
			}()
		}

		for {
			select {
			case <-restart:
				watcherCount--
				if watcherCount == 0 {
					w.Wait()
					config.Since = 1 * time.Second
					watcherCount = len(namespaces)
					continue OUTER
				}
			case <-ctx.Done():
				break OUTER
			}
		}
	}
	<-ctx.Done()
	return nil
}
