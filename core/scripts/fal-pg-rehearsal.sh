#!/usr/bin/env bash
# Synthetic-only isolated PostgreSQL backup/migration rehearsal. No provider calls.
# Usage: bash scripts/fal-pg-rehearsal.sh [baseline-core-dir]
set -euo pipefail
CORE=$(cd "$(dirname "$0")/.." && pwd)
BASE=${1:-/tmp/fal-p1-baseline/core}
OUT=$(mktemp -d /tmp/fal-pg-rehearsal.XXXXXX)
CONTAINER=fal-p1-pg-rehearsal
# Deliberately fixed loopback endpoint and disposable database names; no env DSN accepted.
PORT=55473
OLD=fal_old_${RANDOM}_$$
NEW=fal_restored_${RANDOM}_$$
export GOCACHE=/tmp/fal-go-cache
unset SQL_DSN LOG_SQL_DSN
exec > >(tee "$OUT/run.log") 2>&1
printf 'Artifacts: %s\nBaseline: %s\nCurrent: %s\n' "$OUT" "$BASE" "$CORE"
docker inspect "$CONTAINER" --format '{{.Config.Image}} {{json .HostConfig.PortBindings}}'
cp -R "$BASE" "$OUT/old"
cp -R "$CORE" "$OUT/current"
mkdir -p "$OUT/old/cmd/falrehearsal" "$OUT/current/cmd/falrehearsal"
cat > "$OUT/common.go" <<'GO'
package main
import (
 "fmt"
 "os"
 "time"
 "github.com/labring/aiproxy/core/model"
 "github.com/labring/aiproxy/core/relay/mode"
 "gorm.io/gorm"
)
func must(err error) { if err != nil { panic(err) } }
func check(ok bool, label string) { if !ok { panic(label) }; fmt.Println("PASS", label) }
func main() {
 db,err:=model.OpenPostgreSQL(os.Args[1]);must(err)
 model.DB=db;model.LogDB=db
 switch os.Args[2] {
 case "migrate": migrate(db)
 case "seed":
  now:=time.Now().UTC().Truncate(time.Microsecond)
  log:=model.Log{RequestID:"synthetic-video",Model:"synthetic-video-model",GroupID:"synthetic-group",TokenID:7,Mode:int(mode.Videos),Code:200,RequestAt:now,CreatedAt:now,AsyncUsageStatus:model.AsyncUsageStatusCompleted,Usage:model.Usage{OutputTokens:123},Amount:model.Amount{UsedAmount:0.0123}}
  must(db.Create(&log).Error)
  info:=model.AsyncUsageInfo{RequestID:"synthetic-video",UpstreamID:"synthetic-provider-video",GroupID:"synthetic-group",TokenID:7,Model:"synthetic-video-model",Mode:int(mode.Videos),Status:model.AsyncUsageStatusCompleted,RequestAt:now,CreatedAt:now,UpdatedAt:now,Usage:model.Usage{OutputTokens:123},Amount:model.Amount{UsedAmount:0.0123}}
  must(db.Create(&info).Error)
  video:=model.StoreV2{ChannelID:9,ID:model.VideoJobStoreID("synthetic-provider-video"),GroupID:"synthetic-group",TokenID:7,Model:"synthetic-video-model",Metadata:`{"id":"synthetic-provider-video","status":"completed","url":"https://example.invalid/video.mp4"}`,ExpiresAt:now.Add(24*time.Hour)}
  must(db.Create(&video).Error);fmt.Println("PASS synthetic video store, log, usage seeded")
 case "read":
  var log model.Log;must(db.First(&log,"request_id = ?","synthetic-video").Error)
  var info model.AsyncUsageInfo;must(db.First(&info,"request_id = ?","synthetic-video").Error)
  var video model.StoreV2;must(db.First(&video,"id = ?",model.VideoJobStoreID("synthetic-provider-video")).Error)
  check(log.Usage.OutputTokens==123 && info.Amount.UsedAmount==0.0123 && video.Model=="synthetic-video-model","old models read video/log/usage values")
  view,err:=model.FindGroupVideoTaskByRequestID("synthetic-group","synthetic-video");must(err);check(view!=nil,"old video task service reads new schema")
 case "image": image(db)
 default: panic("unknown command")
 }
}
func migrate(db *gorm.DB) {
 must(db.AutoMigrate(&model.Channel{},&model.ChannelTest{},&model.Token{},&model.PublicMCP{},&model.GroupModelConfig{},&model.PublicMCPReusingParam{},&model.GroupMCP{},&model.Group{},&model.Option{},&model.ModelConfig{}))
 must(db.AutoMigrate(&model.Log{},&model.RequestDetail{},&model.RetryLog{},&model.GroupSummary{},&model.Summary{},&model.ConsumeError{},&model.AsyncUsageInfo{},IMAGE_TABLE &model.StoreV2{},&model.SummaryMinute{},&model.GroupSummaryMinute{}))
 must(model.CreateLogIndexes(db));must(model.CreateSummaryIndexs(db));must(model.CreateGroupSummaryIndexs(db));must(model.CreateSummaryMinuteIndexs(db));must(model.CreateGroupSummaryMinuteIndexs(db))
 fmt.Println("PASS all InitDB/InitLogDB AutoMigrate tables and synchronous custom indexes")
}
GO
sed 's/IMAGE_TABLE //' "$OUT/common.go" > "$OUT/old/cmd/falrehearsal/main.go"
printf '\nfunc image(_ *gorm.DB) {}\n' >> "$OUT/old/cmd/falrehearsal/main.go"
sed 's/IMAGE_TABLE /\&model.ImageTask{},/' "$OUT/common.go" > "$OUT/current/cmd/falrehearsal/main.go"
cat >> "$OUT/current/cmd/falrehearsal/main.go" <<'GO'
func image(db *gorm.DB) {
 task:=&model.ImageTask{ID:"synthetic-image",GroupID:"synthetic-group",TokenID:7,Model:"synthetic-image-model",Fingerprint:"synthetic-fingerprint"}
 info:=&model.AsyncUsageInfo{RequestID:task.ID,GroupID:task.GroupID,TokenID:task.TokenID,Model:task.Model,RequestAt:time.Now()}
 _,created,err:=model.ReserveImageTask(task,info);must(err);check(created,"first reservation created")
 _,created,err=model.ReserveImageTask(task,&model.AsyncUsageInfo{});must(err);check(!created,"reservation replay creates nothing")
 conflict:=*task;conflict.Fingerprint="different";_,_,err=model.ReserveImageTask(&conflict,&model.AsyncUsageInfo{});check(err==model.ErrImageTaskConflict,"conflicting reservation rejected")
 must(model.AcceptImageTask(task.ID,"synthetic-upstream-image"));check(model.AcceptImageTask(task.ID,"other-upstream")!=nil,"duplicate acceptance rejected without overwrite")
 var persisted model.ImageTask;must(db.First(&persisted,"id = ?",task.ID).Error);check(persisted.UpstreamID=="synthetic-upstream-image","accepted upstream identity preserved")
 must(db.First(info,task.UsageID).Error)
 now:=time.Now().Add(time.Second);claimed,err:=model.TryClaimAsyncUsageInfo(info,"synthetic-claim",now.Add(time.Minute),now);must(err);check(claimed,"pending image usage claimed")
 usage:=model.Usage{ImageOutputTokens:4};amount:=model.Amount{UsedAmount:0.04,ImageOutputAmount:0.04}
 must(model.PrepareClaimedAsyncUsageSettlement(info,usage,model.UsageContext{},amount))
 must(model.MarkAsyncUsageBalanceConsumeAttempted(info));must(model.MarkAsyncUsageBalanceConsumed(info));info.BalanceConsumed=true
 settled,err:=model.CompleteClaimedAsyncUsageInfo(info,usage,model.UsageContext{},amount);must(err);check(settled,"first settlement commits")
 settled,err=model.CompleteClaimedAsyncUsageInfo(info,usage,model.UsageContext{},amount);must(err);check(!settled,"settlement replay has zero affected rows")
 must(model.SetImageTaskResult(task.ID,"completed",[]model.ImageOutput{{URL:"https://example.invalid/image.png"}},nil))
 must(model.SetImageTaskResult(task.ID,"failed",nil,&model.ImageTaskError{Code:"late",Message:"ignored"}))
 must(db.First(&persisted,"id = ?",task.ID).Error);check(persisted.Status=="completed","terminal result resists late failure")
 var final model.AsyncUsageInfo;must(db.First(&final,task.UsageID).Error);check(final.Status==model.AsyncUsageStatusCompleted && final.Amount.UsedAmount==0.04 && final.BalanceConsumed,"settlement values persisted")
 for _,table:=range []string{"image_tasks","async_usage_infos","logs"} { var count int64;key:="request_id";if table=="image_tasks" {key="id"};must(db.Table(table).Where(key+" = ?",task.ID).Count(&count).Error);check(count==1,"exactly one image row in "+table) }
}
GO
(cd "$OUT/old" && go build -o "$OUT/old-runner" ./cmd/falrehearsal)
(cd "$OUT/current" && go build -o "$OUT/current-runner" ./cmd/falrehearsal)
for name in "$OLD" "$NEW"; do docker exec "$CONTAINER" createdb -U postgres "$name"; done
DSN_OLD="postgres://postgres@127.0.0.1:$PORT/$OLD?sslmode=disable"
DSN_NEW="postgres://postgres@127.0.0.1:$PORT/$NEW?sslmode=disable"
"$OUT/old-runner" "$DSN_OLD" migrate > "$OUT/baseline-migrate.log" 2>&1
"$OUT/old-runner" "$DSN_OLD" seed
stamp() { python3 -c 'from datetime import datetime,timezone; print(datetime.now(timezone.utc).isoformat())'; }
start=$(stamp)
"$OUT/old-runner" "$DSN_OLD" migrate > "$OUT/baseline-repeat.log" 2>&1
docker logs --since "$start" "$CONTAINER" > "$OUT/baseline-repeat-ddl.log" 2>&1
# Plain JSON projections of old columns remain comparable after additive columns.
snapshot() {
 docker exec "$CONTAINER" psql -XAt -U postgres -d "$1" -c "SELECT (to_jsonb(t)-'log_id'-'image_task_id')::text FROM async_usage_infos t WHERE request_id='synthetic-video'; SELECT to_jsonb(t)::text FROM logs t WHERE request_id='synthetic-video'; SELECT to_jsonb(t)::text FROM store_v2 t WHERE model='synthetic-video-model';"
}
schema() { docker exec "$CONTAINER" pg_dump -U postgres -d "$1" --schema-only --no-owner --no-privileges | sed '/^\\restrict /d; /^\\unrestrict /d'; }
snapshot "$OLD" > "$OUT/old-data.jsonl"
schema "$OLD" > "$OUT/old-schema.sql"
docker exec "$CONTAINER" pg_dump -U postgres -d "$OLD" -Fc > "$OUT/backup.dump"
docker exec -i "$CONTAINER" pg_restore -U postgres -d "$NEW" --exit-on-error < "$OUT/backup.dump"
snapshot "$NEW" > "$OUT/restored-data.jsonl"
cmp "$OUT/old-data.jsonl" "$OUT/restored-data.jsonl"
echo 'PASS pg_dump / pg_restore into separate database preserves synthetic rows'
"$OUT/old-runner" "$DSN_NEW" read
for pass in 1 2; do
 start=$(stamp)
 "$OUT/current-runner" "$DSN_NEW" migrate > "$OUT/migration-$pass.log" 2>&1
 docker logs --since "$start" "$CONTAINER" > "$OUT/ddl-$pass.log" 2>&1
 schema "$NEW" > "$OUT/schema-$pass.sql"
 snapshot "$NEW" > "$OUT/data-$pass.jsonl"
 cmp "$OUT/old-data.jsonl" "$OUT/data-$pass.jsonl"
 echo "PASS migration $pass preserves all synthetic old row columns"
done
cmp "$OUT/schema-1.sql" "$OUT/schema-2.sql"
echo 'PASS second migration leaves identical normalized schema'
diff -u "$OUT/old-schema.sql" "$OUT/schema-1.sql" > "$OUT/schema-diff.patch" || test "$?" = 1
"$OUT/current-runner" "$DSN_NEW" image
"$OUT/old-runner" "$DSN_NEW" read
snapshot "$NEW" > "$OUT/final-old-data.jsonl"
cmp "$OUT/old-data.jsonl" "$OUT/final-old-data.jsonl"
printf 'PASS rehearsal complete. Synthetic-only; no external wallet/provider calls.\nDatabases retained: %s %s\nArtifacts: %s\n' "$OLD" "$NEW" "$OUT"
