import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../api/api_exception.dart';
import '../feedback/feedback_host.dart';
import '../models/fault.dart';
import '../models/overview.dart';
import '../state/data_providers.dart';
import '../state/providers.dart';
import '../widgets/common.dart';
import '../widgets/device_card.dart';
import '../widgets/tiles.dart';

/// 首页：设备状态卡 + 未恢复故障 + 未读入口 + 状态横幅。
class HomePage extends ConsumerStatefulWidget {
  const HomePage({super.key});

  @override
  ConsumerState<HomePage> createState() => _HomePageState();
}

class _HomePageState extends ConsumerState<HomePage> {
  @override
  void initState() {
    super.initState();
    // 进入首页先追平本地缓存（幂等，已在跑则跳过），再刷新 overview。
    WidgetsBinding.instance.addPostFrameCallback((_) => _refreshAll());
  }

  Future<void> _refreshAll() async {
    try {
      await ref.read(syncCoordinatorProvider).syncAll();
    } catch (_) {}
    if (mounted) {
      await ref.read(overviewProvider.notifier).refresh();
    }
  }

  @override
  Widget build(BuildContext context) {
    final overview = ref.watch(overviewProvider);
    final online = ref.watch(onlineProvider).value ?? true;
    final service = ref.watch(serviceStateProvider).value;
    // 原生服务推送状态时触发增量同步（内部有 _running 去重，不重叠执行）。
    ref.listen(serviceStateProvider, (prev, next) {
      if (next.hasValue) {
        unawaited(ref.read(syncCoordinatorProvider).syncAll().catchError((_) => -1));
      }
    });

    final overviewError = overview.error;
    final hubUnreachable = online &&
        ((overviewError is ApiNetworkException) ||
            (overview.value?.hubReachableHint == false) ||
            (service != null && service.running && !service.hubReachable));
    final dnd = overview.value?.dnd;
    final dndActive = (service?.dndActive ?? false) ||
        (dnd?.isActiveAt(DateTime.now()) ?? false);

    return Scaffold(
      appBar: AppBar(
        title: const Text('Assistant'),
        actions: [
          IconButton(
            tooltip: '反馈',
            icon: const Icon(Icons.feedback_outlined),
            onPressed: () => openFeedbackEntry(context, ref),
          ),
          _UnreadAction(),
          IconButton(
            tooltip: '接入管理',
            icon: const Icon(Icons.lan_outlined),
            onPressed: () => context.push('/sources'),
          ),
          IconButton(
            tooltip: '设置',
            icon: const Icon(Icons.settings_outlined),
            onPressed: () => context.push('/settings'),
          ),
        ],
      ),
      body: Column(
        children: [
          if (!online)
            const StatusBanner(
              icon: Icons.wifi_off_outlined,
              text: '手机当前无网络连接，显示本地缓存数据',
              color: Color(0xFFE8960C),
            )
          else if (hubUnreachable)
            StatusBanner(
              icon: Icons.cloud_off_outlined,
              text: service?.lastError?.isNotEmpty == true
                  ? '中枢不可达：${service!.lastError}'
                  : '中枢不可达，通知服务将继续重试',
              color: Theme.of(context).colorScheme.error,
            ),
          if (dndActive && dnd != null)
            StatusBanner(
              icon: Icons.do_not_disturb_on_outlined,
              text: '免打扰中（${dnd.start}–${dnd.end} ${dnd.timezone}），'
                  '通知将静默并在结束后汇总${(service?.heldCount ?? 0) > 0 ? '，已静默 ${service!.heldCount} 条' : ''}',
              color: Theme.of(context).colorScheme.tertiary,
            ),
          Expanded(
            child: RefreshIndicator(
              onRefresh: _refreshAll,
              child: AsyncValueView(
                value: overview,
                onRetry: _refreshAll,
                errorBuilder: (error) => error is ApiNetworkException
                    ? _OfflineHome(onRetry: _refreshAll)
                    : ErrorView(error: error, onRetry: _refreshAll),
                builder: (data) => _HomeBody(
                  data: data,
                  onRefresh: _refreshAll,
                ),
              ),
            ),
          ),
        ],
      ),
    );
  }
}

class _UnreadAction extends ConsumerWidget {
  const _UnreadAction();

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final unread = ref.watch(unreadCountProvider).value ?? 0;
    return IconButton(
      tooltip: '历史消息',
      onPressed: () => context.push('/history'),
      icon: Badge(
        isLabelVisible: unread > 0,
        label: Text('$unread'),
        child: const Icon(Icons.inbox_outlined),
      ),
    );
  }
}

class _HomeBody extends StatelessWidget {
  const _HomeBody({required this.data, required this.onRefresh});

  final Overview data;
  final Future<void> Function() onRefresh;

  @override
  Widget build(BuildContext context) {
    final devices = data.sources.where((s) => s.isDevice).toList();
    final others = data.sources.where((s) => !s.isDevice).toList();
    return ListView(
      padding: const EdgeInsets.only(bottom: 32),
      children: [
        const SectionTitle(text: '设备状态'),
        Padding(
          padding: const EdgeInsets.symmetric(horizontal: 16),
          child: Column(
            children: [
              for (final s in devices) ...[
                DeviceCard(
                  source: s,
                  onTap: () => context.push(
                      '/trends/${Uri.encodeComponent(s.id)}?name=${Uri.encodeComponent(s.name)}'),
                ),
                const SizedBox(height: 12),
              ],
              for (final s in others) ...[
                DeviceCard(source: s),
                const SizedBox(height: 12),
              ],
              if (data.sources.isEmpty)
                const Card(
                  child: Padding(
                    padding: EdgeInsets.all(24),
                    child: Text('还没有接入来源，去「接入管理」创建'),
                  ),
                ),
            ],
          ),
        ),
        SectionTitle(
          text: '未恢复故障',
          trailing: data.openFaults.isEmpty
              ? null
              : Text('${data.openFaults.length} 个',
                  style: Theme.of(context).textTheme.bodySmall),
        ),
        Card(
          margin: const EdgeInsets.symmetric(horizontal: 16),
          child: data.openFaults.isEmpty
              ? const Padding(
                  padding: EdgeInsets.all(24),
                  child: Row(
                    children: [
                      Icon(Icons.check_circle_outline, color: Color(0xFF2E9E5B)),
                      SizedBox(width: 8),
                      Text('当前没有未恢复的故障'),
                    ],
                  ),
                )
              : Column(
                  children: [
                    for (final f in data.openFaults)
                      FaultTile(
                        fault: f,
                        onTap: () => context
                            .push('/faults/${Uri.encodeComponent(f.id)}'),
                      ),
                  ],
                ),
        ),
        const SectionTitle(text: '消息'),
        Card(
          margin: const EdgeInsets.symmetric(horizontal: 16),
          child: ListTile(
            leading: Badge(
              isLabelVisible: data.unreadMessages > 0,
              label: Text('${data.unreadMessages}'),
              child: const Icon(Icons.mark_chat_unread_outlined),
            ),
            title: Text(data.unreadMessages > 0
                ? '${data.unreadMessages} 条未读消息'
                : '没有未读消息'),
            subtitle: const Text('查看全部历史消息'),
            trailing: const Icon(Icons.chevron_right),
            onTap: () => context.push('/history'),
          ),
        ),
      ],
    );
  }
}

/// 中枢不可达时的首页兜底：仍展示本地缓存的故障/来源摘要。
class _OfflineHome extends ConsumerWidget {
  const _OfflineHome({required this.onRetry});

  final Future<void> Function() onRetry;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    return FutureBuilder<List<Fault>>(
      future: ref.read(cacheProvider).openFaults(),
      builder: (context, snap) {
        final faults = snap.data ?? const <Fault>[];
        return ListView(
          padding: const EdgeInsets.only(bottom: 32),
          children: [
            const SizedBox(height: 24),
            Center(
              child: Column(
                children: [
                  Icon(Icons.cloud_off_outlined,
                      size: 40,
                      color: Theme.of(context).colorScheme.outline),
                  const SizedBox(height: 8),
                  Text('中枢暂时不可达，以下为本地缓存',
                      style: Theme.of(context).textTheme.bodyMedium),
                  TextButton.icon(
                    onPressed: () => onRetry(),
                    icon: const Icon(Icons.refresh),
                    label: const Text('重试'),
                  ),
                ],
              ),
            ),
            if (faults.isNotEmpty) ...[
              const SectionTitle(text: '未恢复故障（缓存）'),
              Card(
                margin: const EdgeInsets.symmetric(horizontal: 16),
                child: Column(
                  children: [
                    for (final f in faults)
                      FaultTile(
                        fault: f,
                        onTap: () => context
                            .push('/faults/${Uri.encodeComponent(f.id)}'),
                      ),
                  ],
                ),
              ),
            ],
          ],
        );
      },
    );
  }
}
