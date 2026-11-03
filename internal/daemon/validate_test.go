package daemon

import (
	"context"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestManifestValidate(t *testing.T) {
	s := &ManifestServer{}
	doc := "intent: shop\nmembers:\n  web:\n    kind: container\n    image: ${IMAGE}\n"
	reply, err := s.Validate(context.Background(), &anvilv1.ManifestValidateRequest{Manifest: []byte(doc), Vars: map[string]string{"IMAGE": "nginx"}})
	if err != nil || len(reply.GetProblems()) != 0 {
		t.Fatalf("valid manifest: %v, %v", reply.GetProblems(), err)
	}
	reply, err = s.Validate(context.Background(), &anvilv1.ManifestValidateRequest{Manifest: []byte(doc)})
	if err != nil || len(reply.GetProblems()) != 1 || reply.GetProblems()[0].GetLine() != 5 {
		t.Fatalf("unset var: %v, %v, want one problem on line 5", reply.GetProblems(), err)
	}
}

func TestCloudInitValidate(t *testing.T) {
	s := &CloudInitServer{}
	reply, err := s.Validate(context.Background(), &anvilv1.CloudInitValidateRequest{Content: "#cloud-config\nruncmd: reboot\nfoo: 1\n"})
	if err != nil {
		t.Fatal(err)
	}
	var errs, warns int
	for _, p := range reply.GetProblems() {
		if p.GetWarning() {
			warns++
		} else {
			errs++
		}
	}
	if errs != 1 || warns != 1 {
		t.Fatalf("problems = %v, want one error and one warning", reply.GetProblems())
	}
}
