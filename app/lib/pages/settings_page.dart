import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../api/api_exception.dart';
import '../feedback/feedback_host.dart';
import '../models/service_state.dart';
import '../models/settings.dart';
import '../state/data_providers.dart';
import '../state/providers.dart';
import '../widgets/common.dart';
import '../widgets/format.dart';

/// 设置页：服务状态 / 免打扰 / 告警规则入口 / 权限引导 / 调试 / 账号登出。
class SettingsPage extends ConsumerWidget {
  const SettingsPage({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    return Scaffold(
      appBar: AppBar(title: const Text('设置')),
      body: ListView(
        padding: const EdgeInsets.only(bottom: 48),
        children: const [
          _ServiceSection(),
          _DndSection(),
          _RulesSection(),
          _PermissionSection(),
          _FeedbackSection(),
          _DebugSection(),
          _AccountSection(),
        ],
      ),
    );
  }
}

// ---------------- 服务状态 ----------------

class _ServiceSection extends ConsumerWidget {
  const _ServiceSection();

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final service = ref.watch(serviceStateProvider);
    final state = service.value ?? ServiceState.stopped;
    final scheme = Theme.of(context).colorScheme;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        const SectionTitle(text: '同步服务'),
        Card(
          margin: const EdgeInsets.symmetric(horizontal: 16),
          child: Padding(
            padding: const EdgeInsets.all(16),
            child: Column(
              children: [
                _StateRow(
                  label: '运行状态',
                  value: state.running ? '运行中' : '未运行',
                  valueColor:
                      state.running ? const Color(0xFF2E9E5B) : scheme.outline,
                ),
                _StateRow(
                  label: '中枢连接',
                  value: !state.running
                      ? '—'
                      : state.hubReachable
                          ? '已连接'
                          : '不可达',
                  valueColor: !state.running
                      ? scheme.outline
                      : state.hubReachable
                          ? const Color(0xFF2E9E5B)
                          : scheme.error,
                ),
                _StateRow(
                    label: '最近同步', value: formatTime(state.lastSyncAt)),
                _StateRow(label: '变更序号', value: '${state.lastChangeSeq}'),
                if (state.dndActive)
                  _StateRow(
                      label: '免打扰',
                      value: '生效中，已静默 ${state.heldCount} 条'),
                if (state.lastError != null && state.lastError!.isNotEmpty)
                  _StateRow(label: '最近错误', value: state.lastError!),
                const SizedBox(height: 12),
                Row(
                  children: [
                    Expanded(
                      child: FilledButton.tonalIcon(
                        onPressed: () async {
                          await ref.read(nativeBridgeProvider).startService();
                          ref.invalidate(serviceStateProvider);
                        },
                        icon: const Icon(Icons.play_arrow),
                        label: const Text('启动服务'),
                      ),
                    ),
                    const SizedBox(width: 12),
                    Expanded(
                      child: OutlinedButton.icon(
                        onPressed: () async {
                          await ref.read(nativeBridgeProvider).stopService();
                          ref.invalidate(serviceStateProvider);
                        },
                        icon: const Icon(Icons.stop),
                        label: const Text('停止服务'),
                      ),
                    ),
                  ],
                ),
              ],
            ),
          ),
        ),
      ],
    );
  }
}

class _StateRow extends StatelessWidget {
  const _StateRow({required this.label, required this.value, this.valueColor});

  final String label;
  final String value;
  final Color? valueColor;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 4),
      child: Row(
        children: [
          Text(label,
              style: Theme.of(context)
                  .textTheme
                  .bodyMedium
                  ?.copyWith(color: Theme.of(context).colorScheme.outline)),
          const Spacer(),
          Flexible(
            child: Text(
              value,
              textAlign: TextAlign.end,
              style: Theme.of(context)
                  .textTheme
                  .bodyMedium
                  ?.copyWith(color: valueColor),
            ),
          ),
        ],
      ),
    );
  }
}

// ---------------- 免打扰 ----------------

class _DndSection extends ConsumerStatefulWidget {
  const _DndSection();

  @override
  ConsumerState<_DndSection> createState() => _DndSectionState();
}

class _DndSectionState extends ConsumerState<_DndSection> {
  bool _saving = false;

  Future<void> _save(DndSettings dnd) async {
    final settings = ref.read(settingsProvider).value;
    if (settings == null) return;
    setState(() => _saving = true);
    try {
      final updated = await ref.read(apiProvider).patchSettings(
            expectedVersion: settings.version,
            dnd: dnd,
          );
      await ref.read(cacheProvider).saveSettings(updated);
      ref.read(cacheRevisionProvider.notifier).bump();
      ref.invalidate(settingsProvider);
      // 下发原生配置副本（幂等）。
      unawaited(ref.read(sessionProvider.notifier).pushNativeConfig(
          reportIntervalSeconds: updated.reportIntervalSeconds,
          dnd: updated.dnd));
    } on ApiException catch (e) {
      if (mounted) {
        if (e.isVersionConflict) {
          ScaffoldMessenger.of(context).showSnackBar(
            const SnackBar(content: Text('设置已在别处更新，请刷新后重试')),
          );
          ref.invalidate(settingsProvider);
        } else {
          ScaffoldMessenger.of(context)
              .showSnackBar(SnackBar(content: Text('保存失败：${e.message}')));
        }
      }
    } on ApiNetworkException {
      if (mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(const SnackBar(content: Text('无法连接中枢')));
      }
    } finally {
      if (mounted) setState(() => _saving = false);
    }
  }

  Future<String?> _pickTime(String current) async {
    final parts = current.split(':');
    final initial = TimeOfDay(
      hour: int.tryParse(parts.first) ?? 23,
      minute: parts.length > 1 ? int.tryParse(parts[1]) ?? 0 : 0,
    );
    final picked = await showTimePicker(context: context, initialTime: initial);
    if (picked == null) return null;
    return '${picked.hour.toString().padLeft(2, '0')}:'
        '${picked.minute.toString().padLeft(2, '0')}';
  }

  @override
  Widget build(BuildContext context) {
    final settings = ref.watch(settingsProvider);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        const SectionTitle(text: '免打扰'),
        Card(
          margin: const EdgeInsets.symmetric(horizontal: 16),
          child: AsyncValueView(
            value: settings,
            onRetry: () => ref.invalidate(settingsProvider),
            loading: const Padding(
              padding: EdgeInsets.all(24),
              child: Center(child: CircularProgressIndicator()),
            ),
            builder: (s) {
              final dnd = s.dnd;
              final activeNow = dnd.isActiveAt(DateTime.now());
              return Column(
                children: [
                  SwitchListTile(
                    title: const Text('启用免打扰'),
                    subtitle: Text(
                      activeNow
                          ? '当前生效中：通知静默，结束后汇总（执行在系统服务）'
                          : '窗口内通知静默，结束后汇总（执行在系统服务）',
                    ),
                    value: dnd.enabled,
                    onChanged: _saving
                        ? null
                        : (v) => _save(dnd.copyWith(enabled: v)),
                  ),
                  ListTile(
                    enabled: dnd.enabled && !_saving,
                    leading: const Icon(Icons.schedule_outlined),
                    title: Text('${dnd.start} – ${dnd.end}'),
                    subtitle: Text('时区 ${dnd.timezone}'),
                    trailing: Row(
                      mainAxisSize: MainAxisSize.min,
                      children: [
                        TextButton(
                          onPressed: dnd.enabled && !_saving
                              ? () async {
                                  final t = await _pickTime(dnd.start);
                                  if (t != null) {
                                    await _save(dnd.copyWith(start: t));
                                  }
                                }
                              : null,
                          child: const Text('开始'),
                        ),
                        TextButton(
                          onPressed: dnd.enabled && !_saving
                              ? () async {
                                  final t = await _pickTime(dnd.end);
                                  if (t != null) {
                                    await _save(dnd.copyWith(end: t));
                                  }
                                }
                              : null,
                          child: const Text('结束'),
                        ),
                      ],
                    ),
                  ),
                ],
              );
            },
          ),
        ),
      ],
    );
  }
}

// ---------------- 告警规则入口 ----------------

class _RulesSection extends ConsumerWidget {
  const _RulesSection();

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final rules = ref.watch(rulesProvider).value;
    final enabledCount =
        rules?.rules.where((r) => r.enabled).length ?? 0;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        const SectionTitle(text: '告警规则'),
        Card(
          margin: const EdgeInsets.symmetric(horizontal: 16),
          child: ListTile(
            leading: const Icon(Icons.tune),
            title: Text(rules == null
                ? '告警规则'
                : '心跳 ${rules.heartbeatSeconds}s · $enabledCount/${rules.rules.length} 条启用'),
            subtitle: const Text('阈值 / 容器 / SMART / 存储池 / 心跳规则'),
            trailing: const Icon(Icons.chevron_right),
            onTap: () => context.push('/settings/rules'),
          ),
        ),
      ],
    );
  }
}

// ---------------- 权限引导 ----------------

class _PermissionSection extends ConsumerWidget {
  const _PermissionSection();

  Future<void> _open(
    BuildContext context,
    WidgetRef ref,
    Future<bool> Function() opener,
    String title,
    List<String> steps,
  ) async {
    final ok = await opener();
    if (!ok && context.mounted) {
      await _showGuide(context, ref, title, steps);
    }
  }

  Future<void> _showGuide(BuildContext context, WidgetRef ref, String title,
      List<String> steps) async {
    final info = await ref.read(nativeBridgeProvider).getDeviceInfo();
    if (!context.mounted) return;
    await showDialog<void>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: Text(title),
        content: SingleChildScrollView(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            mainAxisSize: MainAxisSize.min,
            children: [
              if (info.miui != null || info.isMiui)
                Text('检测到 ${info.manufacturer} ${info.model}'
                    '${info.miui != null ? '（${info.miui}）' : ''}，请按以下路径手动开启：',
                    style: Theme.of(ctx).textTheme.bodySmall),
              const SizedBox(height: 8),
              for (var i = 0; i < steps.length; i++)
                Padding(
                  padding: const EdgeInsets.symmetric(vertical: 3),
                  child: Row(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Text('${i + 1}. ',
                          style: Theme.of(ctx).textTheme.bodyMedium),
                      Expanded(
                          child: Text(steps[i],
                              style: Theme.of(ctx).textTheme.bodyMedium)),
                    ],
                  ),
                ),
            ],
          ),
        ),
        actions: [
          FilledButton(
              onPressed: () => Navigator.of(ctx).pop(),
              child: const Text('知道了')),
        ],
      ),
    );
  }

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final bridge = ref.read(nativeBridgeProvider);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        const SectionTitle(text: '权限与保活'),
        Card(
          margin: const EdgeInsets.symmetric(horizontal: 16),
          child: Column(
            children: [
              ListTile(
                leading: const Icon(Icons.notifications_outlined),
                title: const Text('通知权限'),
                subtitle: const Text('接收消息与故障通知'),
                trailing: const Icon(Icons.chevron_right),
                onTap: () => _open(
                  context,
                  ref,
                  bridge.openNotificationSettings,
                  '手动开启通知权限',
                  const [
                    '打开系统「设置」→「通知与状态栏」→「通知管理」',
                    '找到 Assistant，允许通知（含锁屏与横幅）',
                  ],
                ),
              ),
              const Divider(height: 1, indent: 56),
              ListTile(
                leading: const Icon(Icons.battery_saver_outlined),
                title: const Text('忽略电池优化'),
                subtitle: const Text('避免系统休眠导致同步中断'),
                trailing: const Icon(Icons.chevron_right),
                onTap: () => _open(
                  context,
                  ref,
                  bridge.openBatteryOptimizationSettings,
                  '手动关闭电池优化',
                  const [
                    '打开系统「设置」→「应用设置」→「应用管理」→ Assistant',
                    '进入「省电策略」，选择「无限制」',
                  ],
                ),
              ),
              const Divider(height: 1, indent: 56),
              ListTile(
                leading: const Icon(Icons.restart_alt_outlined),
                title: const Text('自启动'),
                subtitle: const Text('MIUI 设备建议开启，重启后自动恢复服务'),
                trailing: const Icon(Icons.chevron_right),
                onTap: () => _open(
                  context,
                  ref,
                  bridge.openAutostartSettings,
                  '手动开启自启动',
                  const [
                    '打开系统「设置」→「应用设置」→「授权管理」→「自启动管理」',
                    '找到 Assistant 并开启',
                    '或在「手机管家」→「应用管理」→ Assistant 中允许自启动',
                  ],
                ),
              ),
            ],
          ),
        ),
        const Padding(
          padding: EdgeInsets.fromLTRB(24, 8, 24, 0),
          child: Text('「强行停止」应用后系统不再自动拉起服务，需手动打开应用恢复。',
              style: TextStyle(fontSize: 12, color: Colors.grey)),
        ),
      ],
    );
  }
}

// ---------------- 反馈 ----------------

/// 反馈设置：Feedback 服务地址（与组件面板内「服务器设置」同一槽位）+
/// 提交入口 + 管理入口。服务地址留空 = 未配置（入口给引导）。
class _FeedbackSection extends ConsumerStatefulWidget {
  const _FeedbackSection();

  @override
  ConsumerState<_FeedbackSection> createState() => _FeedbackSectionState();
}

class _FeedbackSectionState extends ConsumerState<_FeedbackSection> {
  bool _saving = false;

  Future<void> _edit(String? current) async {
    final controller = TextEditingController(text: current ?? '');
    final saved = await showDialog<String>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('Feedback 服务地址'),
        content: TextField(
          controller: controller,
          keyboardType: TextInputType.url,
          autofocus: true,
          decoration: const InputDecoration(
            labelText: '服务地址',
            hintText: 'https://feedback.example.com',
            helperText: '留空并保存可清除配置',
          ),
          onSubmitted: (_) =>
              Navigator.of(ctx).pop(controller.text.trim()),
        ),
        actions: [
          TextButton(
              onPressed: () => Navigator.of(ctx).pop(),
              child: const Text('取消')),
          FilledButton(
              onPressed: () =>
                  Navigator.of(ctx).pop(controller.text.trim()),
              child: const Text('保存')),
        ],
      ),
    );
    if (saved == null || !mounted) return;
    setState(() => _saving = true);
    try {
      final notifier = ref.read(feedbackServerProvider.notifier);
      if (saved.isEmpty) {
        await notifier.clear();
      } else {
        final err = await notifier.setServer(saved);
        if (err != null && mounted) {
          ScaffoldMessenger.of(context)
              .showSnackBar(SnackBar(content: Text(err)));
        }
      }
    } finally {
      if (mounted) setState(() => _saving = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final server = ref.watch(feedbackServerProvider);
    final configured = server.value != null;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        const SectionTitle(text: '反馈'),
        Card(
          margin: const EdgeInsets.symmetric(horizontal: 16),
          child: Column(
            children: [
              ListTile(
                leading: Icon(Icons.dns_outlined,
                    color: theme.colorScheme.primary),
                title: const Text('Feedback 服务'),
                subtitle: server.isLoading
                    ? const Text('读取中…')
                    : Text(
                        configured ? server.value! : '未配置',
                        maxLines: 1,
                        overflow: TextOverflow.ellipsis,
                      ),
                trailing: _saving
                    ? const SizedBox(
                        width: 18,
                        height: 18,
                        child: CircularProgressIndicator(strokeWidth: 2))
                    : const Icon(Icons.edit_outlined),
                onTap: _saving ? null : () => _edit(server.value),
              ),
              const Divider(height: 1, indent: 56),
              ListTile(
                leading: Icon(Icons.feedback_outlined,
                    color: theme.colorScheme.primary),
                title: const Text('提交反馈'),
                subtitle: Text(
                  configured ? '截图标注 / 描述问题 / 附带日志' : '未配置服务',
                  style: theme.textTheme.bodySmall
                      ?.copyWith(color: theme.colorScheme.outline),
                ),
                onTap: () => openFeedbackEntry(context, ref),
              ),
              const Divider(height: 1, indent: 56),
              ListTile(
                leading: Icon(Icons.forum_outlined,
                    color: theme.colorScheme.primary),
                title: const Text('反馈管理'),
                subtitle: Text('收件箱 / 归档 / 回收站',
                    style: theme.textTheme.bodySmall
                        ?.copyWith(color: theme.colorScheme.outline)),
                trailing: const Icon(Icons.chevron_right),
                onTap: () => context.push('/feedback'),
              ),
            ],
          ),
        ),
      ],
    );
  }
}

// ---------------- 调试 ----------------

class _DebugSection extends ConsumerStatefulWidget {
  const _DebugSection();

  @override
  ConsumerState<_DebugSection> createState() => _DebugSectionState();
}

class _DebugSectionState extends ConsumerState<_DebugSection> {
  bool _enabled = false;

  @override
  Widget build(BuildContext context) {
    return Card(
      margin: const EdgeInsets.fromLTRB(16, 16, 16, 0),
      child: SwitchListTile(
        secondary: const Icon(Icons.bug_report_outlined),
        title: const Text('原生诊断日志'),
        subtitle: const Text('通知到达计时等调试用（仅排查时开启）'),
        value: _enabled,
        onChanged: (v) async {
          setState(() => _enabled = v);
          await ref
              .read(nativeBridgeProvider)
              .setDebugLogging(enabled: v);
        },
      ),
    );
  }
}

// ---------------- 账号 ----------------

class _AccountSection extends ConsumerWidget {
  const _AccountSection();

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final session = ref.watch(sessionProvider);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        const SectionTitle(text: '账号'),
        Card(
          margin: const EdgeInsets.symmetric(horizontal: 16),
          child: Column(
            children: [
              ListTile(
                leading: const Icon(Icons.person_outline),
                title: Text(session?.username ?? '未登录'),
                subtitle: Text(session?.hubUrl ?? ''),
              ),
              const Divider(height: 1, indent: 56),
              ListTile(
                leading: Icon(Icons.logout,
                    color: Theme.of(context).colorScheme.error),
                title: Text('登出',
                    style:
                        TextStyle(color: Theme.of(context).colorScheme.error)),
                onTap: () async {
                  final ok = await showDialog<bool>(
                    context: context,
                    builder: (ctx) => AlertDialog(
                      title: const Text('登出'),
                      content: const Text('将停止同步服务并清除本机令牌，确定登出？'),
                      actions: [
                        TextButton(
                            onPressed: () => Navigator.of(ctx).pop(false),
                            child: const Text('取消')),
                        FilledButton(
                            onPressed: () => Navigator.of(ctx).pop(true),
                            child: const Text('登出')),
                      ],
                    ),
                  );
                  if (ok == true) {
                    await ref.read(sessionProvider.notifier).logout();
                  }
                },
              ),
            ],
          ),
        ),
      ],
    );
  }
}
