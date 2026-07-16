package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeDescProtosUsesSlashInSourcePaths(t *testing.T) {
	root := t.TempDir()
	descDir := filepath.Join(root, "desc")
	protoDir := filepath.Join(descDir, "chat")
	if err := os.MkdirAll(protoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protoDir, "conversation.proto"), []byte(`syntax = "proto3";
package chat;
message Conversation {}
service ChatService {
  rpc GetConversation(Conversation) returns (Conversation);
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	outputPath := filepath.Join(root, "chat.proto")
	if err := MergeDescProtos(descDir, outputPath, "chat"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "from desc/chat/conversation.proto") {
		t.Fatalf("generated source path is not slash-normalized:\n%s", content)
	}
}
