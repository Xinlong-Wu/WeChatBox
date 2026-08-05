package tools

import (
	"strings"
	"testing"
)

func TestDocsCreateSpecDelegatesFailureUniquenessCheckToReadOnlyTools(t *testing.T) {
	description := docsCreateSpec().Description
	for _, required := range []string{"do not submit another create request", "feishu_docs_search", "feishu_docs_read", "exactly one match", "never repeats the Feishu create API"} {
		if !strings.Contains(description, required) {
			t.Fatalf("create description missing %q: %s", required, description)
		}
	}
}

func TestDocsFolderCreateSpecDelegatesFailureUniquenessCheckToReadOnlyTool(t *testing.T) {
	description := docsFolderCreateSpec().Description
	for _, required := range []string{"do not submit another create request", "feishu_docs_folder_list", "exactly one matching folder", "never repeats the Feishu create API"} {
		if !strings.Contains(description, required) {
			t.Fatalf("folder create description missing %q: %s", required, description)
		}
	}
}
