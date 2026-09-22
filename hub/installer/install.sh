#!/usr/bin/env bash
# The installer accepts no credential flags or credential environment variables.
set +x
set -Eeuo pipefail
umask 077

# Regression tests rewrite these constants in a disposable COPY, never the host.
INSTALL_ROOT=''
WAIT_SECONDS=120
STABILITY_SECONDS=3
SERVICE=assistant-agent.service
root_path() { printf '%s%s' "$INSTALL_ROOT" "$1"; }
ETC_DIR=$(root_path /etc/assistant-agent)
STATE_DIR=$(root_path /var/lib/assistant-agent)
UNIT_FILE=$(root_path /etc/systemd/system/assistant-agent.service)
BIN_PATH=$(root_path /usr/local/bin/assistant-agent)
ENV_FILE=$ETC_DIR/agent.env
BINDING_FILE=$ETC_DIR/source.json
LOCK_FILE=$(root_path /run/assistant-agent-install.lock)
TMP_DIR='' TRANSACTION=0 KEEP_BACKUP=0
OLD_ACTIVE=0 OLD_ENABLED=disabled
HUB='' VERSION='' RECONFIGURE=0

die() { printf '安装失败：%s\n' "$*" >&2; exit 1; }
usage() { printf '用法：bash install.sh --hub https://host [--version X.Y.Z] [--reconfigure]\n'; }
valid_version() { [[ ${#1} -le 64 && $1 =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; }
valid_key() { [[ $1 =~ ^ask_[0-9a-f]{48}$ ]]; }
valid_source() { [[ $1 =~ ^src_[A-Za-z0-9_-]+$ ]]; }
valid_url() {
  # Deliberately restrict unescaped characters to those safe in EnvironmentFile.
  [[ $1 =~ ^https://([A-Za-z0-9.-]+|\[[A-Fa-f0-9:]+\])(:[0-9]{1,5})?(/[A-Za-z0-9._~%/-]*)?$ ]] &&
    [[ $1 != *'/../'* && $1 != */.. && $1 != *'/./'* && $1 != */. && $1 != *'//'*'//'* ]]
}
atomic_copy() {
  local src=$1 dest=$2
  local staged
  staged=$(mktemp "${dest}.install.XXXXXX") || return 1
  if cp -p "$src" "$staged" && mv -f "$staged" "$dest"; then return 0; fi
  rm -f "$staged"
  return 1
}
# shellcheck disable=SC2329 # Called by the EXIT handler.
rollback() {
  local failed=0 f name
  if [[ -f $UNIT_FILE ]]; then
    systemctl stop "$SERVICE" >/dev/null 2>&1 || failed=1
  fi
  # Disable before removing a freshly installed unit, so no dangling enable link remains.
  if [[ -f $UNIT_FILE ]]; then
    systemctl disable "$SERVICE" >/dev/null 2>&1 || failed=1
  fi
  for f in "$BIN_PATH" "$ENV_FILE" "$UNIT_FILE"; do
    name=$(basename "$f")
    if [[ -f $TMP_DIR/backup/$name ]]; then
      atomic_copy "$TMP_DIR/backup/$name" "$f" || failed=1
    else
      rm -f "$f" || failed=1
    fi
  done
  systemctl daemon-reload >/dev/null 2>&1 || failed=1
  case $OLD_ENABLED in
    enabled) systemctl enable "$SERVICE" >/dev/null 2>&1 || failed=1 ;;
    enabled-runtime) systemctl enable --runtime "$SERVICE" >/dev/null 2>&1 || failed=1 ;;
  esac
  if ((OLD_ACTIVE)); then systemctl start "$SERVICE" >/dev/null 2>&1 || failed=1; fi
  if ((failed)); then
    KEEP_BACKUP=1
    printf '自动恢复未全部成功，备份保留于 %s/backup；请检查 systemd，持久队列未删除。\n' "$TMP_DIR" >&2
  else
    printf '已恢复安装前的程序、配置和服务状态；持久队列未删除。\n' >&2
  fi
}
# shellcheck disable=SC2329 # Invoked by trap on every exit.
finish() {
  local rc=$?
  trap - EXIT INT TERM HUP
  if ((TRANSACTION)); then
    set +e
    rollback
    rc=1
  fi
  if [[ -n $TMP_DIR && $KEEP_BACKUP -eq 0 ]]; then rm -rf "$TMP_DIR"; fi
  exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

while (($#)); do
  case $1 in
    --hub) (($#>=2)) || die '--hub 需要地址'; HUB=$2; shift 2 ;;
    --version) (($#>=2)) || die '--version 需要版本'; VERSION=$2; shift 2 ;;
    --reconfigure) RECONFIGURE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ -n $HUB ]] || { usage >&2; exit 2; }
valid_url "$HUB" || die '中枢地址须为 HTTPS，不能包含用户信息、查询、片段或特殊字符'
HUB=${HUB%/}
[[ -z $VERSION ]] || valid_version "$VERSION" || die '版本必须是正式 X.Y.Z'
for tool in curl jq sha256sum flock systemctl uname id mktemp mkdir chmod cp mv rm cmp cat grep wc awk timeout basename dirname find tr sleep stat; do
  command -v "$tool" >/dev/null 2>&1 || die "缺少依赖 $tool，请由管理员安装后重试"
done
[[ $(uname -s) == Linux ]] || die '只支持 Linux'
[[ ${EUID:-$(id -u)} -eq 0 ]] || die '需要 root，请使用 sudo'
[[ -d $(root_path /run/systemd/system) ]] || die '需要正在运行的 systemd'
case $(uname -m) in x86_64|amd64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; *) die '仅支持 amd64 / arm64' ;; esac

for f in "$ETC_DIR" "$STATE_DIR" "$ENV_FILE" "$BINDING_FILE" "$UNIT_FILE" "$BIN_PATH" "$LOCK_FILE"; do
  [[ ! -L $f ]] || die '安装目标存在符号链接，已停止覆盖'
done
for f in "$ENV_FILE" "$BINDING_FILE" "$UNIT_FILE" "$BIN_PATH"; do
  [[ ! -e $f || -f $f ]] || die '安装目标不是普通文件'
done
# Existing privileged configuration must not be owned by an unprivileged user.
# INSTALL_ROOT is a literal empty string in the distributed script.
if [[ -z $INSTALL_ROOT ]]; then
  for f in "$ETC_DIR" "$ENV_FILE" "$BINDING_FILE" "$UNIT_FILE" "$BIN_PATH"; do
    if [[ -e $f ]]; then
      [[ $(stat -c '%u' "$f") == 0 ]] || die '已有安装文件不属于 root，已停止覆盖'
      mode=$(stat -c '%a' "$f")
      (( (8#$mode & 0022) == 0 )) || die '已有安装文件允许非 root 写入，已停止覆盖'
    fi
  done
fi
# Only the lock is created before all validation; existing files/directories remain untouched.
exec 9>"$LOCK_FILE" || die '无法创建安装锁'
flock -n 9 || die '另一个安装正在进行'
TMP_DIR=$(mktemp -d /tmp/assistant-agent-install.XXXXXX)
chmod 700 "$TMP_DIR"

managed_unit() {
  cat <<UNIT
[Unit]
Description=Assistant Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
EnvironmentFile=$ENV_FILE
ExecStart=$BIN_PATH
Restart=always
RestartSec=5
TimeoutStopSec=20
UMask=0077
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
UNIT
}
legacy_unit() {
  cat <<UNIT
[Unit]
Description=Assistant Agent
After=network-online.target

[Service]
Environment=ASSIST_HUB_URL=$OLD_HUB
Environment=ASSIST_SOURCE_KEY=$OLD_KEY
ExecStart=/usr/local/bin/assistant-agent
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
}
OLD_HUB='' OLD_KEY='' OLD_SOURCE=''
if [[ -f $ENV_FILE ]]; then
  declare -a seen=()
  while IFS= read -r line || [[ -n $line ]]; do
    [[ -z $line || $line == \#* ]] && continue
    field=${line%%=*};value=${line#*=}
    [[ $line == *=* ]] || die '配置格式未知，已停止覆盖'
    for previous in "${seen[@]}"; do [[ $field != "$previous" ]] || die '配置存在重复字段，已停止覆盖'; done
    seen+=("$field")
    case $field in
      ASSIST_HUB_URL) valid_url "$value" || die '现有中枢地址格式未知'; OLD_HUB=${value%/} ;;
      ASSIST_SOURCE_KEY) valid_key "$value" || die '现有密钥格式未知'; OLD_KEY=$value ;;
      ASSIST_SOURCE_ID) valid_source "$value" || die '现有来源 ID 格式未知'; OLD_SOURCE=$value ;;
      ASSIST_QUEUE_DIR) [[ $value == /var/lib/assistant-agent ]] || die '现有队列目录非默认路径，须人工核对，不能重置队列' ;;
      *) die '现有配置含自定义字段，已停止覆盖以保留配置' ;;
    esac
  done <"$ENV_FILE"
  [[ -n $OLD_HUB && -n $OLD_KEY ]] || die '已有配置不完整，已停止覆盖'
fi
# This non-secret binding survives a first-install rollback, so a queue created
# by a failed new process can be retried safely without forgetting its owner.
if [[ -f $BINDING_FILE ]]; then
  jq -e 'type == "object" and (.hub|type == "string") and (.sourceId|type == "string")' "$BINDING_FILE" >/dev/null || die '来源归属记录损坏，已停止覆盖'
  bound_hub=$(jq -r '.hub' "$BINDING_FILE")
  bound_source=$(jq -r '.sourceId' "$BINDING_FILE")
  valid_url "$bound_hub" || die '来源归属记录的中枢地址非法'
  valid_source "$bound_source" || die '来源归属记录的 ID 非法'
  [[ -z $OLD_HUB || $OLD_HUB == "$bound_hub" ]] || die '配置与来源归属记录不一致'
  [[ -z $OLD_SOURCE || $OLD_SOURCE == "$bound_source" ]] || die '配置与来源归属记录不一致'
  OLD_HUB=$bound_hub; OLD_SOURCE=$bound_source
fi
FRAGMENT=$(systemctl show "$SERVICE" --property=FragmentPath --value) || die '无法查询 systemd 服务来源'
DROPINS=$(systemctl show "$SERVICE" --property=DropInPaths --value) || die '无法查询 systemd drop-in'
[[ -z $DROPINS && ! -d $UNIT_FILE.d ]] || die '现有服务含自定义 drop-in，已停止覆盖'
[[ -z $FRAGMENT || $FRAGMENT == "$UNIT_FILE" ]] || die '现有服务来自其他路径，已停止覆盖'
if [[ -f $UNIT_FILE ]]; then
  if [[ -f $ENV_FILE ]]; then
    managed_unit >"$TMP_DIR/expected.unit"
  else
    # Extract only literals, then compare the ENTIRE old auto-generated unit.
    while IFS= read -r line || [[ -n $line ]]; do
      case $line in
        Environment=ASSIST_HUB_URL=*) OLD_HUB=${line#Environment=ASSIST_HUB_URL=} ;;
        Environment=ASSIST_SOURCE_KEY=*) OLD_KEY=${line#Environment=ASSIST_SOURCE_KEY=} ;;
      esac
    done <"$UNIT_FILE"
    valid_url "$OLD_HUB" || die '不能识别旧版自动生成的服务地址'
    valid_key "$OLD_KEY" || die '不能识别旧版自动生成的服务密钥'
    legacy_unit >"$TMP_DIR/expected.unit"
  fi
  cmp -s "$UNIT_FILE" "$TMP_DIR/expected.unit" || die '现有服务含自定义内容，已停止覆盖'
fi
[[ -z $OLD_HUB || ${OLD_HUB%/} == "$HUB" ]] || die '中枢与现有配置不同，不能混用已有队列'
if [[ -z $OLD_SOURCE && -z $OLD_KEY && -d $STATE_DIR && -n $(find "$STATE_DIR" -mindepth 1 -maxdepth 1 -print -quit) ]]; then
  die '发现无归属配置的持久队列，须人工核对来源后再安装'
fi

# curl reads credentials from a private config file; no key appears in argv/logs.
query_status() {
  local key=$1 budget=${2:-15}
  printf 'header = "Authorization: Bearer %s"\n' "$key" >"$TMP_DIR/curl.conf"
  chmod 600 "$TMP_DIR/curl.conf"
  STATUS=
  STATUS=$(curl -q --proto '=https' --max-redirs 0 -fsS --connect-timeout 5 --max-time "$budget" --config "$TMP_DIR/curl.conf" "$HUB/api/v1/agent/status" 2>/dev/null) || return 1
  jq -e 'type == "object" and (.sourceId|type == "string" and test("^src_[A-Za-z0-9_-]+$")) and (.lastMetricsSeq|type == "number" and . >= 0 and floor == .) and (.agentVersion == null or (.agentVersion|type == "string"))' >/dev/null 2>&1 <<<"$STATUS"
}
KEY=$OLD_KEY
if [[ -n $OLD_KEY && -z $OLD_SOURCE ]]; then
  query_status "$OLD_KEY" || die '旧安装尚无来源绑定且旧密钥无法验证；请先核对来源，保留队列，不要重建'
  OLD_SOURCE=$(jq -r '.sourceId' <<<"$STATUS")
fi
if ((RECONFIGURE)) || [[ -z $KEY ]]; then
  printf '请粘贴设备来源密钥（输入不回显）： ' >/dev/tty || die '需要交互终端输入密钥'
  IFS= read -rs KEY </dev/tty || die '未读取到密钥'
  printf '\n' >/dev/tty
fi
valid_key "$KEY" || die '来源密钥格式错误'
query_status "$KEY" || die '来源密钥无法认证或中枢暂不可达；现有安装未切换'
SOURCE_ID=$(jq -r '.sourceId' <<<"$STATUS")
[[ -z $OLD_SOURCE || $OLD_SOURCE == "$SOURCE_ID" ]] || die '新密钥属于不同来源，拒绝混用现有队列'
if [[ $(jq -r '.lastMetricsSeq' <<<"$STATUS") != 0 && ! -f $STATE_DIR/agent-queue.db ]]; then
  die '来源已有指标但本机缺少原持久队列，不能从头重置序号；请恢复队列或创建独立来源'
fi

MANIFEST=$TMP_DIR/manifest.json
MANIFEST_URL=$HUB/api/v1/agent/releases/stable
[[ -z $VERSION ]] || MANIFEST_URL=$HUB/api/v1/agent/releases/$VERSION/agent-manifest.json
curl -q --proto '=https' --max-redirs 0 -fsS --connect-timeout 10 --max-time 120 --max-filesize 1048576 "$MANIFEST_URL" -o "$MANIFEST" || die '无法下载版本清单'
jq -e '.schemaVersion == 1 and (.version|type == "string") and (.assets|type == "array" and length == 2) and ([.assets[].arch]|sort == ["amd64","arm64"]) and all(.assets[]; .os == "linux" and (.size|type == "number" and . > 0 and . <= 134217728 and floor == .) and (.sha256|type == "string" and test("^[0-9a-f]{64}$")) and (.name|type == "string"))' "$MANIFEST" >/dev/null || die '版本清单格式非法'
TARGET_VERSION=$(jq -r '.version' "$MANIFEST")
valid_version "$TARGET_VERSION" || die '清单版本非法'
[[ -z $VERSION || $VERSION == "$TARGET_VERSION" ]] || die '清单版本与指定版本不一致'
jq -e --arg v "$TARGET_VERSION" 'all(.assets[]; .name == ("assistant-agent_" + $v + "_linux_" + .arch))' "$MANIFEST" >/dev/null || die '清单文件名不匹配'
ASSET=$(jq -r --arg a "$ARCH" '.assets[]|select(.arch==$a)|.name' "$MANIFEST")
ASSET_SIZE=$(jq -r --arg a "$ARCH" '.assets[]|select(.arch==$a)|.size' "$MANIFEST")
ASSET_SHA=$(jq -r --arg a "$ARCH" '.assets[]|select(.arch==$a)|.sha256' "$MANIFEST")
BIN_TMP=$TMP_DIR/agent
curl -q --proto '=https' --max-redirs 0 -fsS --connect-timeout 10 --max-time 120 --max-filesize "$ASSET_SIZE" "$HUB/api/v1/agent/releases/$TARGET_VERSION/$ASSET" -o "$BIN_TMP" || die '无法下载 Agent'
[[ $(wc -c <"$BIN_TMP" | tr -d '[:space:]') == "$ASSET_SIZE" ]] || die 'Agent 文件大小不匹配'
[[ $(sha256sum "$BIN_TMP" | awk '{print $1}') == "$ASSET_SHA" ]] || die 'Agent SHA256 不匹配'
chmod 755 "$BIN_TMP"
NEW_VERSION=$(timeout 10 "$BIN_TMP" --version) || die '下载的程序无法执行'
[[ $NEW_VERSION == "assistant-agent $TARGET_VERSION (linux/$ARCH)" ]] || die '下载的程序内部版本或架构不匹配'
semver_lt() {
  jq -ne --arg a "$1" --arg b "$2" '($a|split(".")|map(tonumber)) < ($b|split(".")|map(tonumber))' >/dev/null
}
if [[ -e $BIN_PATH ]]; then
  OLD_VERSION_OUTPUT=$(timeout 10 "$BIN_PATH" --version) || die '现有程序无法识别，已停止覆盖'
  CURRENT_VERSION=${OLD_VERSION_OUTPUT#assistant-agent }; CURRENT_VERSION=${CURRENT_VERSION%% *}
  valid_version "$CURRENT_VERSION" || die '现有程序版本无法识别'
  if semver_lt "$TARGET_VERSION" "$CURRENT_VERSION"; then die '拒绝降级，现有程序和队列保持不变'; fi
fi

mkdir -p "$TMP_DIR/backup"
for f in "$BIN_PATH" "$ENV_FILE" "$UNIT_FILE"; do
  if [[ -f $f ]]; then cp -p "$f" "$TMP_DIR/backup/$(basename "$f")"; fi
 done
if systemctl is-active --quiet "$SERVICE"; then OLD_ACTIVE=1; fi
OLD_ENABLED=$(systemctl is-enabled "$SERVICE" 2>/dev/null) || true
case $OLD_ENABLED in enabled|enabled-runtime|disabled|not-found|'') ;; *) die '服务启用方式不受支持，须人工核对' ;; esac

# All destructive changes below are protected by the EXIT/signal transaction.
TRANSACTION=1
if [[ -n $FRAGMENT || -f $UNIT_FILE ]]; then systemctl stop "$SERVICE" || die '无法停止旧服务'; fi
# Capture baseline AFTER stopping/flushing the previous agent.
query_status "$KEY" || die '停止旧服务后无法取得指标基线，恢复旧安装'
[[ $(jq -r '.sourceId' <<<"$STATUS") == "$SOURCE_ID" ]] || die '来源身份在安装期间变化'
BEFORE_SEQ=$(jq -r '.lastMetricsSeq' <<<"$STATUS")
mkdir -p "$ETC_DIR" "$(dirname "$BIN_PATH")" "$(dirname "$UNIT_FILE")"
chmod 700 "$ETC_DIR"
if [[ ! -e $STATE_DIR ]]; then mkdir -p "$STATE_DIR"; chmod 700 "$STATE_DIR"; fi
[[ -d $STATE_DIR ]] || die '队列路径不是目录'
{
  printf 'ASSIST_HUB_URL=%s\n' "$HUB"
  printf 'ASSIST_SOURCE_KEY=%s\n' "$KEY"
  printf 'ASSIST_QUEUE_DIR=/var/lib/assistant-agent\n'
  printf 'ASSIST_SOURCE_ID=%s\n' "$SOURCE_ID"
} >"$TMP_DIR/new.env"
chmod 600 "$TMP_DIR/new.env"
managed_unit >"$TMP_DIR/new.unit"; chmod 644 "$TMP_DIR/new.unit"
atomic_copy "$BIN_TMP" "$BIN_PATH"
atomic_copy "$TMP_DIR/new.env" "$ENV_FILE"
atomic_copy "$TMP_DIR/new.unit" "$UNIT_FILE"
# Do not remove this binding on rollback: it contains no key and owns any queue
# subsequently written by the new process, even if its configuration is restored.
jq -n --arg hub "$HUB" --arg id "$SOURCE_ID" '{hub:$hub,sourceId:$id}' >"$TMP_DIR/source.json"
chmod 600 "$TMP_DIR/source.json"
atomic_copy "$TMP_DIR/source.json" "$BINDING_FILE"
systemctl daemon-reload
systemctl enable "$SERVICE"
systemctl reset-failed "$SERVICE" >/dev/null 2>&1 || true
systemctl start "$SERVICE"

START_PID='' START_RESTARTS='' STABLE_SINCE=$SECONDS
DEADLINE=$((SECONDS + WAIT_SECONDS))
check_running() {
  local active pid restarts
  active=$(systemctl show "$SERVICE" --property=ActiveState --value) || return 1
  pid=$(systemctl show "$SERVICE" --property=MainPID --value) || return 1
  restarts=$(systemctl show "$SERVICE" --property=NRestarts --value) || return 1
  [[ $active == active && $pid =~ ^[1-9][0-9]*$ && $restarts == 0 ]] || return 1
  if [[ -z $START_PID ]]; then START_PID=$pid; START_RESTARTS=$restarts; fi
  [[ $START_PID == "$pid" && $START_RESTARTS == "$restarts" ]]
}
while ((SECONDS < DEADLINE)); do
  check_running || die '新程序退出或重复重启，正在恢复旧安装'
  remaining=$((DEADLINE - SECONDS))
  ((remaining>0)) || break
  budget=$remaining; ((budget<=5)) || budget=5
  if query_status "$KEY" "$budget"; then
    if jq -e --arg id "$SOURCE_ID" --arg version "$TARGET_VERSION" --argjson before "$BEFORE_SEQ" '.sourceId == $id and .agentVersion == $version and .lastMetricsSeq > $before' >/dev/null <<<"$STATUS"; then
      if ((SECONDS - STABLE_SINCE >= STABILITY_SECONDS)); then
        check_running || die '新程序已退出，正在恢复旧安装'
        TRANSACTION=0
        printf '接入成功：Agent %s 已被中枢接收新指标。\n' "$TARGET_VERSION"
        exit 0
      fi
    fi
  fi
  ((SECONDS < DEADLINE)) && sleep 1
done
check_running || die '新程序启动失败，正在恢复旧安装'
TRANSACTION=0
printf '待确认上报：服务正常运行，但 %s 秒内未确认新指标。保留新服务和队列继续补传；请检查 journalctl -u assistant-agent 和 App 来源状态。\n' "$WAIT_SECONDS" >&2
exit 3
