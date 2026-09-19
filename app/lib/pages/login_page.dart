import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../api/api_exception.dart';
import '../state/providers.dart';

/// 登录页：账号密码 + hub URL（保存以便下次预填）。
class LoginPage extends ConsumerStatefulWidget {
  const LoginPage({super.key});

  @override
  ConsumerState<LoginPage> createState() => _LoginPageState();
}

class _LoginPageState extends ConsumerState<LoginPage> {
  final _formKey = GlobalKey<FormState>();
  late final TextEditingController _hubController;
  late final TextEditingController _userController;
  late final TextEditingController _passController;
  bool _submitting = false;
  bool _obscure = true;
  String? _errorText;

  @override
  void initState() {
    super.initState();
    _hubController = TextEditingController(text: _savedHubUrl());
    _userController = TextEditingController();
    _passController = TextEditingController();
  }

  String _savedHubUrl() {
    final prefs = ref.read(sharedPrefsProvider);
    return prefs.getString('hub.url') ?? '';
  }

  @override
  void dispose() {
    _hubController.dispose();
    _userController.dispose();
    _passController.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    if (!_formKey.currentState!.validate()) return;
    setState(() {
      _submitting = true;
      _errorText = null;
    });
    try {
      await ref.read(sessionProvider.notifier).login(
            hubUrl: _hubController.text,
            username: _userController.text.trim(),
            password: _passController.text,
          );
      // 成功后路由 redirect 自动跳首页。
    } on ApiException catch (e) {
      setState(() {
        _errorText = switch (e.code) {
          'invalid_credentials' || 'unauthorized' => '账号或密码错误',
          'rate_limited' => '尝试过于频繁，请${e.retryAfterSeconds != null ? ' ${e.retryAfterSeconds} 秒后' : '稍后'}再试',
          _ => e.message,
        };
      });
    } on ApiNetworkException {
      setState(() => _errorText = '无法连接中枢，请检查地址与网络');
    } catch (e) {
      setState(() => _errorText = '登录失败：$e');
    } finally {
      if (mounted) setState(() => _submitting = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final scheme = theme.colorScheme;
    return Scaffold(
      body: SafeArea(
        child: Center(
          child: SingleChildScrollView(
            padding: const EdgeInsets.all(24),
            child: ConstrainedBox(
              constraints: const BoxConstraints(maxWidth: 420),
              child: Form(
                key: _formKey,
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: [
                    Icon(Icons.monitor_heart_outlined,
                        size: 56, color: scheme.primary),
                    const SizedBox(height: 16),
                    Text('Assistant',
                        textAlign: TextAlign.center,
                        style: theme.textTheme.headlineMedium),
                    const SizedBox(height: 6),
                    Text('个人助手 · 设备监控与消息',
                        textAlign: TextAlign.center,
                        style: theme.textTheme.bodyMedium
                            ?.copyWith(color: scheme.outline)),
                    const SizedBox(height: 32),
                    TextFormField(
                      controller: _hubController,
                      decoration: const InputDecoration(
                        labelText: '中枢地址',
                        hintText: 'https://hub.example.com',
                        prefixIcon: Icon(Icons.link),
                      ),
                      keyboardType: TextInputType.url,
                      autocorrect: false,
                      validator: (v) =>
                          (v == null || v.trim().isEmpty) ? '请输入中枢地址' : null,
                    ),
                    const SizedBox(height: 16),
                    TextFormField(
                      controller: _userController,
                      decoration: const InputDecoration(
                        labelText: '账号',
                        prefixIcon: Icon(Icons.person_outline),
                      ),
                      autocorrect: false,
                      validator: (v) =>
                          (v == null || v.trim().isEmpty) ? '请输入账号' : null,
                    ),
                    const SizedBox(height: 16),
                    TextFormField(
                      controller: _passController,
                      decoration: InputDecoration(
                        labelText: '密码',
                        prefixIcon: const Icon(Icons.lock_outline),
                        suffixIcon: IconButton(
                          icon: Icon(_obscure
                              ? Icons.visibility_outlined
                              : Icons.visibility_off_outlined),
                          onPressed: () =>
                              setState(() => _obscure = !_obscure),
                        ),
                      ),
                      obscureText: _obscure,
                      validator: (v) =>
                          (v == null || v.isEmpty) ? '请输入密码' : null,
                      onFieldSubmitted: (_) => _submit(),
                    ),
                    if (_errorText != null) ...[
                      const SizedBox(height: 16),
                      Container(
                        padding: const EdgeInsets.all(12),
                        decoration: BoxDecoration(
                          color: scheme.errorContainer,
                          borderRadius: BorderRadius.circular(12),
                        ),
                        child: Row(
                          children: [
                            Icon(Icons.error_outline,
                                size: 18, color: scheme.onErrorContainer),
                            const SizedBox(width: 8),
                            Expanded(
                              child: Text(_errorText!,
                                  style: TextStyle(
                                      color: scheme.onErrorContainer)),
                            ),
                          ],
                        ),
                      ),
                    ],
                    const SizedBox(height: 24),
                    FilledButton(
                      onPressed: _submitting ? null : _submit,
                      style: FilledButton.styleFrom(
                        padding: const EdgeInsets.symmetric(vertical: 16),
                      ),
                      child: _submitting
                          ? const SizedBox(
                              width: 20,
                              height: 20,
                              child: CircularProgressIndicator(strokeWidth: 2),
                            )
                          : const Text('登录'),
                    ),
                  ],
                ),
              ),
            ),
          ),
        ),
      ),
    );
  }
}
