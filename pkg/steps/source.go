package steps

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"

	buildapi "github.com/openshift/api/build/v1"
	imageclientset "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	coreapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"

	"github.com/openshift/ci-operator/pkg/api"
)

const (
	CiAnnotationPrefix = "ci.openshift.io"
	PersistsLabel      = "persists-between-builds"
	JobLabel           = "job"
	BuildIdLabel       = "build-id"
	CreatesLabel       = "creates"
)

const (
	CreatedByCILabel = "created-by-ci"
)

var (
	JobSpecAnnotation = fmt.Sprintf("%s/%s", CiAnnotationPrefix, "job-spec")
)

func sourceDockerfile(fromTag api.PipelineImageStreamTagReference, job *JobSpec) string {
	return fmt.Sprintf(`FROM %s:%s
ENV GIT_COMMITTER_NAME=developer GIT_COMMITTER_EMAIL=developer@redhat.com
ENV REPO_OWNER=%s REPO_NAME=%s PULL_REFS=%s
RUN umask 0002 && /usr/bin/clonerefs --src-root=/go --log=-
WORKDIR /go/src/github.com/%s/%s/
`, PipelineImageStream, fromTag, job.Refs.Org, job.Refs.Repo, job.Refs.String(), job.Refs.Org, job.Refs.Repo)
}

type sourceStep struct {
	config      api.SourceStepConfiguration
	buildClient BuildClient
	istClient   imageclientset.ImageStreamTagInterface
	jobSpec     *JobSpec
}

func (s *sourceStep) Run(dry bool) error {
	dockerfile := sourceDockerfile(s.config.From, s.jobSpec)
	return handleBuild(s.buildClient, buildFromSource(
		s.jobSpec, s.config.From, s.config.To,
		buildapi.BuildSource{
			Type:       buildapi.BuildSourceDockerfile,
			Dockerfile: &dockerfile,
		},
	), dry)
}

func buildFromSource(jobSpec *JobSpec, fromTag, toTag api.PipelineImageStreamTagReference, source buildapi.BuildSource) *buildapi.Build {
	log.Printf("Creating build for %s/%s:%s", jobSpec.Namespace(), PipelineImageStream, toTag)
	layer := buildapi.ImageOptimizationSkipLayers
	build := &buildapi.Build{
		ObjectMeta: meta.ObjectMeta{
			Name:      string(toTag),
			Namespace: jobSpec.Namespace(),
			Labels: map[string]string{
				PersistsLabel:    "false",
				JobLabel:         jobSpec.Job,
				BuildIdLabel:     jobSpec.BuildId,
				CreatesLabel:     string(toTag),
				CreatedByCILabel: "true",
			},
			Annotations: map[string]string{
				JobSpecAnnotation: jobSpec.rawSpec,
			},
		},
		Spec: buildapi.BuildSpec{
			CommonSpec: buildapi.CommonSpec{
				ServiceAccount: "builder", // TODO: remove when build cluster has https://github.com/openshift/origin/pull/17668
				Source:         source,
				Strategy: buildapi.BuildStrategy{
					Type: buildapi.DockerBuildStrategyType,
					DockerStrategy: &buildapi.DockerBuildStrategy{
						From: &coreapi.ObjectReference{
							Kind:      "ImageStreamTag",
							Namespace: jobSpec.Namespace(),
							Name:      fmt.Sprintf("%s:%s", PipelineImageStream, fromTag),
						},
						ForcePull: true,
						NoCache:   true,
						Env: []coreapi.EnvVar{
							{Name: "JOB_SPEC", Value: jobSpec.rawSpec},
						},
						ImageOptimizationPolicy: &layer,
					},
				},
				Output: buildapi.BuildOutput{
					To: &coreapi.ObjectReference{
						Kind:      "ImageStreamTag",
						Namespace: jobSpec.Namespace(),
						Name:      fmt.Sprintf("%s:%s", PipelineImageStream, toTag),
					},
				},
			},
		},
	}
	if owner := jobSpec.Owner(); owner != nil {
		build.OwnerReferences = append(build.OwnerReferences, *owner)
	}

	return build
}

func handleBuild(buildClient BuildClient, build *buildapi.Build, dry bool) error {
	if dry {
		buildJSON, err := json.Marshal(build)
		if err != nil {
			return fmt.Errorf("failed to marshal build: %v", err)
		}
		fmt.Printf("%s\n", buildJSON)
		return nil
	}
	if _, err := buildClient.Create(build); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return waitForBuild(buildClient, build.Name)
}

func waitForBuild(buildClient BuildClient, name string) error {
	log.Printf("Waiting for build %s to finish", name)
	for {
		retry, err := waitForBuildOrTimeout(buildClient, name)
		if err != nil {
			return err
		}
		if !retry {
			break
		}
	}

	return nil
}

func waitForBuildOrTimeout(buildClient BuildClient, name string) (bool, error) {
	isOK := func(b *buildapi.Build) bool {
		return b.Status.Phase == buildapi.BuildPhaseComplete
	}
	isFailed := func(b *buildapi.Build) bool {
		return b.Status.Phase == buildapi.BuildPhaseFailed ||
			b.Status.Phase == buildapi.BuildPhaseCancelled ||
			b.Status.Phase == buildapi.BuildPhaseError
	}
	list, err := buildClient.List(meta.ListOptions{FieldSelector: fields.Set{"metadata.name": name}.AsSelector().String()})
	if err != nil {
		return false, err
	}
	if len(list.Items) != 1 {
		return false, fmt.Errorf("could not find build %s", name)
	}
	build := &list.Items[0]
	if isOK(build) {
		return false, nil
	}
	if isFailed(build) {
		log.Printf("Build %s/%s failed, printing logs:", build.Namespace, build.Name)
		printBuildLogs(buildClient, build.Name)
		return false, fmt.Errorf("the build %s/%s failed with status %q", build.Namespace, build.Name, build.Status.Phase)
	}

	watcher, err := buildClient.Watch(meta.ListOptions{
		FieldSelector: fields.Set{"metadata.name": name}.AsSelector().String(),
		Watch:         true,
	})
	if err != nil {
		return false, err
	}
	defer watcher.Stop()

	for {
		event, ok := <-watcher.ResultChan()
		if !ok {
			// restart
			return true, nil
		}
		if build, ok := event.Object.(*buildapi.Build); ok {
			if isOK(build) {
				return false, nil
			}
			if isFailed(build) {
				log.Printf("Build %s/%s failed, printing logs:", build.Namespace, build.Name)
				printBuildLogs(buildClient, build.Name)
				return false, fmt.Errorf("the build %s/%s failed with status %q", build.Namespace, build.Name, build.Status.Phase)
			}
		}
	}
}

func printBuildLogs(buildClient BuildClient, name string) {
	if s, err := buildClient.Logs(name, &buildapi.BuildLogOptions{
		NoWait:     true,
		Timestamps: true,
	}); err == nil {
		defer s.Close()
		if _, err := io.Copy(os.Stdout, s); err != nil {
			log.Printf("error: Unable to copy log output from failed build: %v", err)
		}
	} else {
		log.Printf("error: Unable to retrieve logs from failed build: %v", err)
	}
}

func (s *sourceStep) Done() (bool, error) {
	return imageStreamTagExists(s.config.To, s.istClient)
}

func imageStreamTagExists(reference api.PipelineImageStreamTagReference, istClient imageclientset.ImageStreamTagInterface) (bool, error) {
	log.Printf("Checking for existence of %s:%s", PipelineImageStream, reference)
	_, err := istClient.Get(
		fmt.Sprintf("%s:%s", PipelineImageStream, reference),
		meta.GetOptions{},
	)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		} else {
			return false, err
		}
	} else {
		return true, nil
	}
}

func (s *sourceStep) Requires() []api.StepLink {
	return []api.StepLink{api.InternalImageLink(s.config.From)}
}

func (s *sourceStep) Creates() []api.StepLink {
	return []api.StepLink{api.InternalImageLink(s.config.To)}
}

func SourceStep(config api.SourceStepConfiguration, buildClient BuildClient, istClient imageclientset.ImageStreamTagInterface, jobSpec *JobSpec) api.Step {
	return &sourceStep{
		config:      config,
		buildClient: buildClient,
		istClient:   istClient,
		jobSpec:     jobSpec,
	}
}
