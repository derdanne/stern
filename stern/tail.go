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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"regexp"
	"text/template"

	"time"

	"github.com/fatih/color"
	"github.com/pkg/errors"
	"gopkg.in/Graylog2/go-gelf.v2/gelf"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

type Tail struct {
	Namespace      string
	PodName        string
	ContainerName  string
	NodeName       string
	Options        *TailOptions
	req            *rest.Request
	closed         chan struct{}
	Active         bool
	podColor       *color.Color
	containerColor *color.Color
	tmpl           *template.Template
}

type TailOptions struct {
	Timestamps    bool
	SinceSeconds  int64
	Exclude       []*regexp.Regexp
	Include       []*regexp.Regexp
	Namespace     bool
	TailLines     *int64
	ContextName   string
	ClusterName   string
	GraylogServer string
}

// NewTail returns a new tail for a Kubernetes container inside a pod
func NewTail(namespace, podName, containerName string, nodeName string, tmpl *template.Template, options *TailOptions) *Tail {
	return &Tail{
		Namespace:     namespace,
		PodName:       podName,
		ContainerName: containerName,
		NodeName:      nodeName,
		Options:       options,
		closed:        make(chan struct{}),
		Active:        true,
		tmpl:          tmpl,
	}
}

var colorList = [][2]*color.Color{
	{color.New(color.FgHiCyan), color.New(color.FgCyan)},
	{color.New(color.FgHiGreen), color.New(color.FgGreen)},
	{color.New(color.FgHiMagenta), color.New(color.FgMagenta)},
	{color.New(color.FgHiYellow), color.New(color.FgYellow)},
	{color.New(color.FgHiBlue), color.New(color.FgBlue)},
	{color.New(color.FgHiRed), color.New(color.FgRed)},
}

func determineColor(podName string) (podColor, containerColor *color.Color) {
	hash := fnv.New32()
	hash.Write([]byte(podName))
	idx := hash.Sum32() % uint32(len(colorList))

	colors := colorList[idx]
	return colors[0], colors[1]
}

// Start starts tailing
func (t *Tail) Start(ctx context.Context, i v1.PodInterface, gelfWriter *gelf.TCPWriter, logC chan<- string) {
	t.podColor, t.containerColor = determineColor(t.PodName)

	go func() {
		g := color.New(color.FgHiGreen, color.Bold).SprintFunc()
		p := t.podColor.SprintFunc()
		c := t.containerColor.SprintFunc()
		if t.Options.Namespace {
			logC <- fmt.Sprintf("%s %s %s › %s\n", g("+"), p(t.Namespace), p(t.PodName), c(t.ContainerName))
		} else {
			logC <- fmt.Sprintf("%s %s › %s\n", g("+"), p(t.PodName), c(t.ContainerName))
		}

		req := i.GetLogs(t.PodName, &corev1.PodLogOptions{
			Follow:       true,
			Timestamps:   t.Options.Timestamps,
			Container:    t.ContainerName,
			SinceSeconds: &t.Options.SinceSeconds,
			TailLines:    t.Options.TailLines,
		})

		stream, err := req.Stream()
		if err != nil {
			fmt.Println(errors.Wrapf(err, "Error opening stream to %s/%s: %s\n", t.Namespace, t.PodName, t.ContainerName))
			t.Active = false
			return
		}
		defer stream.Close()

		go func() {
			<-t.closed
			stream.Close()
			t.Active = false
		}()

		reader := bufio.NewReader(stream)

	OUTER:
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}

			str := string(line)

			for _, rex := range t.Options.Exclude {
				if rex.MatchString(str) {
					continue OUTER
				}
			}

			if len(t.Options.Include) != 0 {
				matches := false
				for _, rin := range t.Options.Include {
					if rin.MatchString(str) {
						matches = true
						break
					}
				}
				if !matches {
					continue OUTER
				}
			}
			logC <- t.Print(str, gelfWriter)
		}
	}()

	go func() {
		<-ctx.Done()
		close(t.closed)
	}()
}

// Close stops tailing
func (t *Tail) Close() {
	r := color.New(color.FgHiRed, color.Bold).SprintFunc()
	p := t.podColor.SprintFunc()
	if t.Options.Namespace {
		fmt.Fprintf(os.Stderr, "%s %s %s\n", r("-"), p(t.Namespace), p(t.PodName))
	} else {
		fmt.Fprintf(os.Stderr, "%s %s\n", r("-"), p(t.PodName))
	}
	close(t.closed)
}

// Build Graylog message
func wrapBuildMessage(s string, f string, l int32, ex map[string]interface{}, h string) *gelf.Message {

	m := &gelf.Message{
		Version:  "1.1",
		Host:     h,
		Short:    s,
		Full:     f,
		TimeUnix: float64(time.Now().Unix()),
		Level:    l,
		Extra:    ex,
	}
	return m
}

// Print prints a color coded log message with the pod and container names
func (t *Tail) Print(msg string, gelfWriter *gelf.TCPWriter) string {
	if t.Options.GraylogServer != "" {
		var c int
		var crop string
		lengthmsg := len(msg)

		customExtras := map[string]interface{}{
			"Namespace":     t.Namespace,
			"PodName":       t.PodName,
			"ContainerName": t.ContainerName,
			"NodeName":      t.NodeName,
		}

		// build gelf short_message
		if lengthmsg < 50 {
			c = lengthmsg - 1
			crop = ""
		} else {
			c = 50
			crop = " ..."
		}
		smsg := "Log event in " + t.Namespace + " from " + t.ContainerName + " in " + t.PodName + " on " + t.NodeName + ": " + msg[0:c] + crop

		// set host if ContextName empty
		var host string
		if t.Options.ClusterName != "" {
			host = t.Options.ClusterName
		} else {
			if t.Options.ContextName == "" {
				host = "default"
			} else {
				host = t.Options.ContextName
			}
		}
		gm := wrapBuildMessage(smsg, msg, 3, customExtras, host)

		writeMsgErr := gelfWriter.WriteMessage(gm)
		if writeMsgErr != nil {
			os.Stderr.WriteString(fmt.Sprintf("Received error when sending GELF message: %s", writeMsgErr.Error()))
		}
		return ""
	} else {
		vm := Log{
			Message:        msg,
			Namespace:      t.Namespace,
			PodName:        t.PodName,
			ContainerName:  t.ContainerName,
			NodeName:       t.NodeName,
			PodColor:       t.podColor,
			ContainerColor: t.containerColor,
		}

		var buf bytes.Buffer
		err := t.tmpl.Execute(&buf, vm)
		if err != nil {
			os.Stderr.WriteString(fmt.Sprintf("expanding template failed: %s", err))
			return ""
		}
		return buf.String()
	}
}

// Log is the object which will be used together with the template to generate
// the output.
type Log struct {
	// Message is the log message itself
	Message string `json:"message"`

	// Namespace of the pod
	Namespace string `json:"namespace"`

	// PodName of the pod
	PodName string `json:"podName"`

	// ContainerName of the container
	ContainerName string `json:"containerName"`

	// Name of the node the pod is running on
	NodeName string `json:"nodeName"`

	PodColor       *color.Color `json:"-"`
	ContainerColor *color.Color `json:"-"`
}
