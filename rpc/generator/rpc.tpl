syntax = "proto3";

package {{.package}};
option go_package="./{{.package}}";

import "buf/validate/validate.proto";

// ──── Common messages (shared across all modules) ────

message Empty {}

message PageInfo {
  uint64 page = 1;
  uint64 page_size = 2;
}

message Timestamp {
  int64 created_at = 1;
  int64 updated_at = 2;
}

message SortField {
  string field = 1;
  string order = 2;  // "asc" or "desc"
}
