package buildkite

import (
	"log"
	"strconv"
	"strings"

	"gopkg.in/buildkite/go-buildkite.v2/buildkite"
)

type Session struct {
	Org    string
	client *buildkite.Client
}

func NewSession(org string, apiToken string) (*Session, error) {
	config, err := buildkite.NewTokenConfig(apiToken, false)
	if err != nil {
		return nil, err
	}
	return &Session{
		Org:    org,
		client: buildkite.NewClient(config.Client()),
	}, nil
}

type VmkiteJob struct {
	ID          string
	BuildNumber string
	Pipeline    string
	Metadata    VmkiteMetadata
}

func (bk *Session) VmkiteJobs() ([]VmkiteJob, error) {
	debugf("Builds.ListByOrg(%s, ...)", bk.Org)
	builds, _, err := bk.client.Builds.ListByOrg(bk.Org, &buildkite.BuildsListOptions{
		State: []string{"scheduled", "running"},
	})
	if err != nil {
		return nil, err
	}
	jobs := make([]VmkiteJob, 0)
	for _, build := range builds {
		for _, job := range build.Jobs {
			metadata := parseAgentQueryRules(job.AgentQueryRules)
			if metadata.GuestID != "" && metadata.VMDK != "" {
				jobs = append(jobs, VmkiteJob{
					ID:          *job.ID,
					BuildNumber: strconv.Itoa(*build.Number),
					Pipeline:    *build.Pipeline.Slug,
					Metadata:    metadata,
				})
			}
		}
	}
	return jobs, nil
}

func (bk *Session) IsFinished(job VmkiteJob) (bool, error) {
	debugf("Builds.Get(%s, %s, %s)", bk.Org, job.Pipeline, job.BuildNumber)
	build, _, err := bk.client.Builds.Get(bk.Org, job.Pipeline, job.BuildNumber)
	if err != nil {
		return false, err
	}
	for _, buildJob := range build.Jobs {
		if *buildJob.ID == job.ID {
			switch *buildJob.State {
			case "scheduled", "running":
				return false, nil
			}
			return true, nil
		}
	}
	return false, nil
}

type VmkiteMetadata struct {
	VMDK    string
	GuestID string
}

func parseAgentQueryRules(rules []string) VmkiteMetadata {
	metadata := VmkiteMetadata{}
	for _, r := range rules {
		parts := strings.SplitN(r, "=", 2)
		if len(parts) == 2 {
			switch parts[0] {
			case "vmkite-vmdk":
				metadata.VMDK = parts[1]
			case "vmkite-guestid":
				metadata.GuestID = parts[1]
			}
		}
	}
	return metadata
}

func debugf(format string, data ...interface{}) {
	log.Printf("[buildkite] "+format, data...)
}
