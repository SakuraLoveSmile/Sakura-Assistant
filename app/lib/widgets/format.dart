import 'package:intl/intl.dart';

/// 时间与数值格式化。数据按 RFC3339 UTC 解析、本地时区显示。

final _fullFormat = DateFormat('yyyy-MM-dd HH:mm:ss');
final _shortFormat = DateFormat('MM-dd HH:mm');

/// 完整本地时间（详情页）。
String formatTime(DateTime? utc) =>
    utc == null ? '—' : _fullFormat.format(utc.toLocal());

/// 短本地时间（列表项）。
String formatTimeShort(DateTime? utc) =>
    utc == null ? '—' : _shortFormat.format(utc.toLocal());

/// 相对时间：刚刚 / n分钟前 / n小时前 / n天前；超过 7 天显示日期。
String formatRelative(DateTime? utc) {
  if (utc == null) return '—';
  final diff = DateTime.now().difference(utc.toLocal());
  if (diff.inSeconds < 60) return '刚刚';
  if (diff.inMinutes < 60) return '${diff.inMinutes} 分钟前';
  if (diff.inHours < 24) return '${diff.inHours} 小时前';
  if (diff.inDays < 7) return '${diff.inDays} 天前';
  return DateFormat('yyyy-MM-dd').format(utc.toLocal());
}

/// 运行时长：123456s → "14 天 6 小时"。
String formatUptime(int? seconds) {
  if (seconds == null) return '—';
  var s = seconds;
  final days = s ~/ 86400;
  s %= 86400;
  final hours = s ~/ 3600;
  s %= 3600;
  final minutes = s ~/ 60;
  if (days > 0) return '$days 天 $hours 小时';
  if (hours > 0) return '$hours 小时 $minutes 分';
  return '$minutes 分钟';
}

String formatBytes(int? bytes) {
  if (bytes == null) return '—';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  var v = bytes.toDouble();
  var unit = 0;
  while (v >= 1024 && unit < units.length - 1) {
    v /= 1024;
    unit++;
  }
  return '${v.toStringAsFixed(unit == 0 ? 0 : 1)} ${units[unit]}';
}

/// 每秒字节速率 → 友好显示。
String formatBps(double? bps) {
  if (bps == null) return '—';
  const units = ['B/s', 'KiB/s', 'MiB/s', 'GiB/s'];
  var v = bps;
  var unit = 0;
  while (v >= 1024 && unit < units.length - 1) {
    v /= 1024;
    unit++;
  }
  return '${v.toStringAsFixed(unit == 0 ? 0 : 1)} ${units[unit]}';
}

String formatPercent(double? p) =>
    p == null ? '—' : '${p.toStringAsFixed(1)}%';

/// 消息/事件 kind → 中文标签（未知 kind 原样显示）。
String kindLabel(String kind) {
  switch (kind) {
    case 'feedback_created':
      return '新反馈';
    case 'feedback_fault':
      return '反馈故障';
    case 'feedback_recovered':
      return '反馈恢复';
    case 'agent_started':
      return '采集启动';
    case 'heartbeat_lost':
      return '心跳失联';
    case 'heartbeat_back':
      return '心跳恢复';
    case 'threshold':
      return '阈值告警';
    case 'threshold_recovered':
      return '阈值恢复';
    case 'container_exit':
      return '容器退出';
    case 'container_back':
      return '容器恢复';
    case 'smart_failing':
      return 'SMART 异常';
    case 'smart_back':
      return 'SMART 恢复';
    case 'pool_error':
      return '存储池异常';
    case 'pool_back':
      return '存储池恢复';
    case 'host_reboot':
      return '主机重启';
    case 'queue_overflow':
      return '队列溢出';
    case 'fault_open':
      return '故障开启';
    case 'custom':
      return '自定义';
    default:
      return kind;
  }
}

// ---------------- 反馈管理（api-v1 §3.1 / feedback-integration §3） ----------------

/// 反馈收件箱生命周期（mgmtState）→ 中文标签。
String feedbackMgmtLabel(String? state) => switch (state) {
      'inbox' => '收件箱',
      'archived' => '已归档',
      'trash' => '回收站',
      _ => (state == null || state.isEmpty) ? '—' : state,
    };

/// 反馈处理状态（上游 status 字段，未知值原样显示）。
String feedbackStatusLabel(String? status) => switch (status) {
      'queued' => '排队中',
      'processing' => '处理中',
      'archived' => '已归档',
      'failed' => '处理失败',
      'needs_review' => '待复核',
      'needs_info' => '待补充信息',
      _ => (status == null || status.isEmpty) ? '—' : status,
    };

/// 问题单状态（issueStatus）→ 中文标签。
String feedbackIssueLabel(String? status) => switch (status) {
      'open' => '待处理',
      'waiting_user' => '等待用户',
      'waiting_admin' => '等待管理员',
      'resolved' => '已解决',
      _ => (status == null || status.isEmpty) ? '—' : status,
    };

/// 收集 / 归档进度（collectionState）→ 中文标签。
String feedbackCollectionLabel(String? state) => switch (state) {
      'waiting_configuration' => '等待配置',
      'waiting_source_confirmation' => '等待来源确认',
      'waiting_manual_archive' => '等待手动归档',
      'queued' => '排队中',
      _ => (state == null || state.isEmpty) ? '—' : state,
    };

/// 归档阶段（archiveStage）→ 中文标签。
String feedbackArchiveStageLabel(String? stage) => switch (stage) {
      'task_pending' => '任务待创建',
      'task_created' => '任务已创建',
      'asset_uploading' => '资产上传中',
      'asset_finalized' => '资产已上传',
      'comment_pending' => '评论待写入',
      'complete' => '归档完成',
      _ => (stage == null || stage.isEmpty) ? '—' : stage,
    };

/// 管理动作 → 中文标签（feedback-integration §4.2 七动作）。
String feedbackActionLabel(String action) => switch (action) {
      'archive' => '归档',
      'unarchive' => '取消归档',
      'trash' => '移入回收站',
      'restore' => '恢复',
      'resume_processing' => '恢复处理',
      'retry' => '重试处理',
      'recheck' => '重新检查',
      _ => action,
    };
