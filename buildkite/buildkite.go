package buildkite

import (
	"log"
	"strings"

	"gopkg.in/buildkite/go-buildkite.v2/buildkite"
)

type Session struct {
	ApiToken string
	Org      string
}

type VmkiteJob struct {
	ID   string
	VMDK string
}

func (bk *Session) VmkiteJobs() ([]VmkiteJob, error) {
	config, err := buildkite.NewTokenConfig(bk.ApiToken, false)
	if err != nil {
		return nil, err
	}
	client := buildkite.NewClient(config.Client())
	buildListOptions := buildkite.BuildsListOptions{
		State: []string{"scheduled", "running"},
	}
	debugf("Builds.ListByOrg(%s, ...)", bk.Org)
	builds, _, err := client.Builds.ListByOrg(bk.Org, &buildListOptions)
	if err != nil {
		return nil, err
	}
	jobs := make([]VmkiteJob, 0)
	for _, build := range builds {
		for _, job := range build.Jobs {
			if vmdk := vmdkFromAgentQueryRules(job.AgentQueryRules); vmdk != "" {
				jobs = append(jobs, VmkiteJob{ID: *job.ID, VMDK: vmdk})
			}
		}
	}
	return jobs, nil
}

func vmdkFromAgentQueryRules(rules []string) string {
	for _, r := range rules {
		parts := strings.SplitN(r, "=", 2)
		if len(parts) == 2 && parts[0] == "vmkite-vmdk" {
			return parts[1]
		}
	}
	return ""
}

func debugf(format string, data ...interface{}) {
	log.Printf("[buildkite] "+format, data...)
}
