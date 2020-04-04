/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/NYTimes/gziphandler"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"

	prowv1 "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/spyglass/api"
)

const PrefixDynamicHandlers = "/dyanmic"

func NewLensServer(
	listenAddress string,
	pjFetcher ProwJobFetcher,
	gcsArtifactFetcher ArtifactFetcher,
	podLogArtifactFetcher ArtifactFetcher,
	cfg config.Getter,
	lenses map[LensOpt]api.Lens,
) (*http.Server, error) {

	mux := http.NewServeMux()

	seenLens := sets.String{}
	for lensOpt, lens := range lenses {
		if seenLens.Has(lensOpt.LensName) {
			return nil, fmt.Errorf("duplicate lens named %q", lensOpt.LensName)
		}
		seenLens.Insert(lensOpt.LensName)

		logrus.WithField("Lens", lensOpt.LensName).Info("Adding handler for lens")
		opt := lensHandlerOpts{
			PJFetcher:             pjFetcher,
			GCSArtifactFetcher:    gcsArtifactFetcher,
			PodLogArtifactFetcher: podLogArtifactFetcher,
			ConfigGetter:          cfg,
			LensOpt:               lensOpt,
		}
		mux.Handle(PrefixDynamicHandlers+"/"+lensOpt.LensName, gziphandler.GzipHandler(newLensHandler(lens, opt)))
	}

	return &http.Server{Addr: listenAddress, Handler: mux}, nil
}

type LensOpt struct {
	TemplateFilesLocation string
	LensResourcesDir      string
	LensName              string
	LensTitle             string
}

type lensHandlerOpts struct {
	PJFetcher             ProwJobFetcher
	GCSArtifactFetcher    ArtifactFetcher
	PodLogArtifactFetcher ArtifactFetcher
	ConfigGetter          config.Getter
	LensOpt
}

func newLensHandler(lens api.Lens, opts lensHandlerOpts) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			writeHTTPError(w, fmt.Errorf("failed to read request body: %w", err), http.StatusInternalServerError)
			return
		}

		request := &api.LensRequest{}
		if err := json.Unmarshal(body, request); err != nil {
			writeHTTPError(w, fmt.Errorf("failed to unmarshal request: %w", err), http.StatusBadRequest)
			return
		}

		artifacts, err := FetchArtifacts(opts.PJFetcher, opts.ConfigGetter, opts.GCSArtifactFetcher, opts.PodLogArtifactFetcher, request.ArtifactSource, "", opts.ConfigGetter().Deck.Spyglass.SizeLimit, request.Artifacts)
		if err != nil {
			writeHTTPError(w, fmt.Errorf("Failed to retrieve expected artifacts: %w", err), http.StatusInternalServerError)
			return
		}

		switch request.Action {
		case api.RequestActionInitial:
			t, err := template.ParseFiles(path.Join(opts.TemplateFilesLocation, "spyglass-lens.html"))
			if err != nil {
				http.Error(w, fmt.Sprintf("Failed to load template: %v", err), http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "text/html; encoding=utf-8")
			t.Execute(w, struct {
				Title   string
				BaseURL string
				Head    template.HTML
				Body    template.HTML
			}{
				opts.LensTitle,
				request.ResourceRoot,
				template.HTML(lens.Header(artifacts, opts.LensResourcesDir, opts.ConfigGetter().Deck.Spyglass.Lenses[request.LensIndex].Lens.Config)),
				template.HTML(lens.Body(artifacts, opts.LensResourcesDir, "", opts.ConfigGetter().Deck.Spyglass.Lenses[request.LensIndex].Lens.Config)),
			})

		case api.RequestActionRerender:
			w.Header().Set("Content-Type", "text/html; encoding=utf-8")
			w.Write([]byte(lens.Body(artifacts, opts.LensResourcesDir, request.Data, opts.ConfigGetter().Deck.Spyglass.Lenses[request.LensIndex].Lens.Config)))

		case api.RequestActionCallBack:
			w.Write([]byte(lens.Callback(artifacts, opts.LensResourcesDir, request.Data, opts.ConfigGetter().Deck.Spyglass.Lenses[request.LensIndex].Lens.Config)))

		default:
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(fmt.Sprintf("Invalid action %q", request.Action)))
		}
	}
}

func writeHTTPError(w http.ResponseWriter, err error, statusCode int) {
	if statusCode == 0 {
		statusCode = http.StatusInternalServerError
	}
	logrus.WithError(err).WithField("statusCode", statusCode).Debug("Failed to process request")
	w.WriteHeader(statusCode)
	if _, err := w.Write([]byte(err.Error())); err != nil {
		logrus.WithError(err).Error("Failed to write response")
	}
}

// ArtifactFetcher knows how to fetch artifacts
type ArtifactFetcher interface {
	Artifact(key string, artifactName string, sizeLimit int64) (api.Artifact, error)
}

// FetchArtifacts fetches artifacts.
// TODO: Unexport once we only have remote lenses
func FetchArtifacts(
	pjFetcher ProwJobFetcher,
	cfg config.Getter,
	gcsArtifactFetcher ArtifactFetcher,
	podLogArtifactFetcher ArtifactFetcher,
	src string,
	podName string,
	sizeLimit int64,
	artifactNames []string,
) ([]api.Artifact, error) {
	artStart := time.Now()
	arts := []api.Artifact{}
	keyType, key, err := splitSrc(src)
	if err != nil {
		return arts, fmt.Errorf("error parsing src: %v", err)
	}
	jobName, buildID, err := keyToJob(src)
	if err != nil {
		return arts, fmt.Errorf("could not derive job: %v", err)
	}
	gcsKey := ""
	switch keyType {
	case api.GCSKeyType:
		gcsKey = strings.TrimSuffix(key, "/")
	case api.ProwKeyType:
		if gcsKey, err = ProwToGCS(pjFetcher, cfg, key); err != nil {
			logrus.Warningln(err)
		}
	default:
		return nil, fmt.Errorf("invalid src: %v", src)
	}

	podLogNeeded := false
	for _, name := range artifactNames {
		art, err := gcsArtifactFetcher.Artifact(gcsKey, name, sizeLimit)
		if err == nil {
			// Actually try making a request, because calling GCSArtifactFetcher.artifact does no I/O.
			// (these files are being explicitly requested and so will presumably soon be accessed, so
			// the extra network I/O should not be too problematic).
			_, err = art.Size()
		}
		if err != nil {
			if name == "build-log.txt" {
				podLogNeeded = true
			}
			continue
		}
		arts = append(arts, art)
	}

	if podLogNeeded {
		art, err := podLogArtifactFetcher.Artifact(jobName, buildID, sizeLimit)
		if err != nil {
			logrus.Errorf("Failed to fetch pod log: %v", err)
		} else {
			arts = append(arts, art)
		}
	}

	logrus.WithField("duration", time.Since(artStart).String()).Infof("Retrieved artifacts for %v", src)
	return arts, nil
}

// ProwJobFetcher knows how to get a ProwJob
type ProwJobFetcher interface {
	GetProwJob(job string, id string) (prowv1.ProwJob, error)
}

// prowToGCS returns the GCS key corresponding to the given prow key
// TODO: Unexport once we only have remote lenses
func ProwToGCS(fetcher ProwJobFetcher, config config.Getter, prowKey string) (string, error) {
	jobName, buildID, err := keyToJob(prowKey)
	if err != nil {
		return "", fmt.Errorf("could not get GCS src: %v", err)
	}

	job, err := fetcher.GetProwJob(jobName, buildID)
	if err != nil {
		return "", fmt.Errorf("Failed to get prow job from src %q: %v", prowKey, err)
	}

	url := job.Status.URL
	prefix := config().Plank.GetJobURLPrefix(job.Spec.Refs)
	if !strings.HasPrefix(url, prefix) {
		return "", fmt.Errorf("unexpected job URL %q when finding GCS path: expected something starting with %q", url, prefix)
	}
	return url[len(prefix):], nil

}

func splitSrc(src string) (keyType, key string, err error) {
	split := strings.SplitN(src, "/", 2)
	if len(split) < 2 {
		err = fmt.Errorf("invalid src %s: expected <key-type>/<key>", src)
		return
	}
	keyType = split[0]
	key = split[1]
	return
}

// keyToJob takes a spyglass URL and returns the jobName and buildID.
func keyToJob(src string) (jobName string, buildID string, err error) {
	src = strings.Trim(src, "/")
	parsed := strings.Split(src, "/")
	if len(parsed) < 2 {
		return "", "", fmt.Errorf("expected at least two path components in %q", src)
	}
	jobName = parsed[len(parsed)-2]
	buildID = parsed[len(parsed)-1]
	return jobName, buildID, nil
}
