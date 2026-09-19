import 'dart:async';

import 'package:feedback_widget/feedback_widget.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_secure_storage/flutter_secure_storage.dart';
import 'package:go_router/go_router.dart';

/// 反馈提交组件（feedback_widget）的宿主集成。
///
/// - 固定 `appId = com.sakurasep.assistant`，`appVersion` 随上报；
/// - 服务地址经组件的 `FeedbackServerPrefStore` + `normalizeServerBase`
///   配置在本机（与面板内「服务器设置」读写同一槽位）；
/// - 登录会话与中枢会话天然隔离：组件令牌键由「有效地址 + appId」派生
///   （`feedbackTokenStorageKey`），切换服务地址后读不到旧槽位令牌；
/// - 面板内登录 / 截图遮挡 / 日志附件均由组件实现，宿主不自采集凭据。

/// 服务端登记的软件标识。
const kFeedbackAppId = 'com.sakurasep.assistant';

/// 随反馈上报的应用版本——与 pubspec.yaml `version:` 的版本名保持同步。
const kAppVersionName = '1.1.0';

/// 组件默认服务地址占位。
///
/// `FeedbackConfig.apiBase` 必填合法 http(s) 地址；本应用要求「先在设置里
/// 配置反馈服务」，因此该默认值只是占位——未配置时入口被「未配置服务」
/// 拦截，组件面板不会被呼出，占位地址不会被真实请求。覆盖槽位键由它
/// 派生（`serverOverrideStorageKey`），改动该值会另开槽位（视为换默认身份）。
const kFeedbackDefaultApiBase = 'http://127.0.0.1:8787';

/// 服务器覆盖偏好槽位键：设置页与组件面板内设置共用同一槽位。
String get feedbackServerSlotKey => serverOverrideStorageKey(
      appId: kFeedbackAppId,
      defaultApiBase: kFeedbackDefaultApiBase,
    );

/// 宿主侧 `FeedbackServerPrefStore`：与组件原生默认实现
/// `SecureServerPrefStore`（src/server_pref_store_io.dart，未导出）同为
/// 默认 `FlutterSecureStorage` + 同一派生键 → 读写同一槽位。
/// 因此组件本体不传 `serverPrefStore`（该参数是 `@visibleForTesting`），
/// 本类仅供设置页读写覆盖值；测试注入 `MemoryServerPrefStore`。
class SecureStorageServerPrefStore implements FeedbackServerPrefStore {
  const SecureStorageServerPrefStore(
      [this._storage = const FlutterSecureStorage()]);

  final FlutterSecureStorage _storage;

  @override
  Future<String?> read(String key) => _storage.read(key: key);

  @override
  Future<void> write(String key, String value) =>
      _storage.write(key: key, value: value);

  @override
  Future<void> delete(String key) => _storage.delete(key: key);
}

/// 服务器覆盖偏好仓库（生产走安全存储；测试替换为内存实现）。
final feedbackServerPrefStoreProvider = Provider<FeedbackServerPrefStore>(
  (ref) => const SecureStorageServerPrefStore(),
);

/// 面板控制器：由应用根持有（路由切换不丢草稿 / 登录态 / 轮询）。
final feedbackControllerProvider = Provider<FeedbackController>((ref) {
  final controller = FeedbackController();
  ref.onDispose(controller.dispose);
  return controller;
});

/// 组件重建计数：服务地址变更后 bump → `FeedbackWidget` 以新 key 重建，
/// 迫使面板重新加载覆盖偏好（组件只在挂载 / 服务身份切换时读槽位）。
final feedbackUiRevisionProvider =
    NotifierProvider<FeedbackUiRevision, int>(FeedbackUiRevision.new);

class FeedbackUiRevision extends Notifier<int> {
  @override
  int build() => 0;

  void bump() => state++;
}

/// 已配置的反馈服务地址（规范化结果；null = 未配置 → 入口引导进设置）。
final feedbackServerProvider =
    AsyncNotifierProvider<FeedbackServerNotifier, String?>(
        FeedbackServerNotifier.new);

class FeedbackServerNotifier extends AsyncNotifier<String?> {
  @override
  Future<String?> build() async {
    try {
      final raw =
          await ref.read(feedbackServerPrefStoreProvider).read(feedbackServerSlotKey);
      final norm = raw == null ? null : normalizeServerBase(raw);
      // 所存值非法（旧格式 / 手改）视为未配置，绝不静默应用。
      return (norm?.ok ?? false) ? norm!.base : null;
    } catch (_) {
      return null; // 安全存储不可用：按未配置处理，入口给「未配置服务」引导。
    }
  }

  /// 保存服务地址（先经 `normalizeServerBase` 规范化校验）。
  /// 成功返回 null；校验失败返回可读原因供 UI 展示。
  Future<String?> setServer(String raw) async {
    final norm = normalizeServerBase(raw);
    if (!norm.ok) return norm.reason ?? '地址无效';
    await ref
        .read(feedbackServerPrefStoreProvider)
        .write(feedbackServerSlotKey, norm.base!);
    state = AsyncData(norm.base);
    ref.read(feedbackUiRevisionProvider.notifier).bump();
    return null;
  }

  /// 清除覆盖（恢复未配置态）。
  Future<void> clear() async {
    await ref.read(feedbackServerPrefStoreProvider).delete(feedbackServerSlotKey);
    state = const AsyncData(null);
    ref.read(feedbackUiRevisionProvider.notifier).bump();
  }

  /// 面板内「服务器设置」改动后由 [FeedbackHost] 回写内存态
  /// （store 已由组件写好，这里只同步展示值；不 bump 重建——面板正在用）。
  void syncEffective(String? base) {
    final v = (base == null || base == kFeedbackDefaultApiBase) ? null : base;
    if (state.value != v) state = AsyncData(v);
  }
}

/// 反馈提交层：经 `MaterialApp.router` 的 `builder` 包在应用 Navigator 之外。
///
/// 组件面板需要 Navigator / Overlay 祖先（输入框选择浮层、`showDialog`），
/// 而应用自己的 Navigator 在组件**之下**（child）——所以外层垫一个只承载
/// 组件、不做页面跳转的 Navigator（与组件官方示例同构）。
class FeedbackHost extends ConsumerStatefulWidget {
  const FeedbackHost({super.key, this.child});

  /// 应用 Navigator 子树（MaterialApp builder 传入的 child）。
  final Widget? child;

  @override
  ConsumerState<FeedbackHost> createState() => _FeedbackHostState();
}

class _FeedbackHostState extends ConsumerState<FeedbackHost> {
  @override
  void initState() {
    super.initState();
    // 组件上报「实际使用地址」变化（面板内改地址 / 恢复默认）→ 回写内存态。
    ref.read(feedbackControllerProvider).addListener(_onEffectiveBase);
  }

  @override
  void dispose() {
    ref
        .read(feedbackControllerProvider)
        .removeListener(_onEffectiveBase);
    super.dispose();
  }

  void _onEffectiveBase() {
    ref
        .read(feedbackServerProvider.notifier)
        .syncEffective(ref.read(feedbackControllerProvider).effectiveApiBase);
  }

  @override
  Widget build(BuildContext context) {
    final controller = ref.watch(feedbackControllerProvider);
    final configured =
        ref.watch(feedbackServerProvider).value != null;
    final revision = ref.watch(feedbackUiRevisionProvider);
    return Navigator(
      onGenerateRoute: (settings) => PageRouteBuilder<void>(
        settings: settings,
        // 外层路由只承载组件，不需要转场动画。
        transitionDuration: Duration.zero,
        reverseTransitionDuration: Duration.zero,
        pageBuilder: (context, animation, secondaryAnimation) =>
            FeedbackWidget(
          // 地址变更 → 新 key 重建组件（面板重新读覆盖偏好；草稿随旧面板卸载）。
          key: ValueKey('feedback-$revision'),
          config: FeedbackConfig(
            kFeedbackDefaultApiBase,
            kFeedbackAppId,
            appName: 'Assistant',
            appVersion: kAppVersionName,
            side: FeedbackSide.right,
            // 已配置才显示侧边悬浮「反馈」入口；未配置时只留宿主入口
            // （点击给「未配置服务」引导，而非把面板指向占位地址）。
            showLauncher: configured,
            launcherMode: FeedbackLauncherMode.tab,
            captureMode: FeedbackCaptureMode.viewport,
            theme: FeedbackThemeMode.system,
          ),
          controller: controller,
          // 不传 serverPrefStore（@visibleForTesting）：组件原生默认
          // SecureServerPrefStore 与本宿主实现同后端同键，读写同一槽位。
          child: widget.child ?? const SizedBox.shrink(),
        ),
      ),
    );
  }
}

/// 「反馈」入口统一逻辑：已配置 → 截图并呼出面板；
/// 未配置 → 「未配置服务」引导进设置页。
Future<void> openFeedbackEntry(BuildContext context, WidgetRef ref) async {
  final configured =
      ref.read(feedbackServerProvider).value != null;
  if (!configured) {
    final go = await showDialog<bool>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('未配置反馈服务'),
        content: const Text('提交反馈前需要先在「设置 → 反馈」中填写 Feedback 服务地址。'),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(ctx).pop(false),
            child: const Text('取消'),
          ),
          FilledButton(
            onPressed: () => Navigator.of(ctx).pop(true),
            child: const Text('去设置'),
          ),
        ],
      ),
    );
    if (go == true && context.mounted) {
      context.push('/settings');
    }
    return;
  }
  // 先截图后开面板（组件草稿守卫：已有草稿 / 截图时不重拍）。
  await ref.read(feedbackControllerProvider).captureAndOpen();
}
