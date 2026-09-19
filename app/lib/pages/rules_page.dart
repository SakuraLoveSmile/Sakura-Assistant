import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../api/api_exception.dart';
import '../models/rules.dart';
import '../state/data_providers.dart';
import '../state/providers.dart';
import '../widgets/common.dart';

/// 告警规则编辑：阈值/forSeconds/recover/heartbeatSeconds/开关 +
/// expectedVersion 乐观锁（version_conflict 提示刷新）。
class RulesPage extends ConsumerStatefulWidget {
  const RulesPage({super.key});

  @override
  ConsumerState<RulesPage> createState() => _RulesPageState();
}

class _RulesPageState extends ConsumerState<RulesPage> {
  final _heartbeatController = TextEditingController();
  List<AlertRule>? _draft;
  int _version = 0;
  bool _saving = false;
  bool _dirty = false;

  void _load(Rules rules) {
    if (_draft != null && _version == rules.version) return;
    _draft = rules.rules;
    _version = rules.version;
    _heartbeatController.text = '${rules.heartbeatSeconds}';
    _dirty = false;
  }

  Future<void> _save() async {
    final draft = _draft;
    if (draft == null) return;
    final heartbeat = int.tryParse(_heartbeatController.text.trim());
    if (heartbeat == null || heartbeat < 60 || heartbeat > 3600) {
      ScaffoldMessenger.of(context).showSnackBar(
          const SnackBar(content: Text('心跳判定需为 60–3600 秒')));
      return;
    }
    setState(() => _saving = true);
    try {
      final updated = await ref.read(apiProvider).putRules(
            expectedVersion: _version,
            heartbeatSeconds: heartbeat,
            rules: draft,
          );
      await ref.read(cacheProvider).saveRules(updated);
      ref.read(cacheRevisionProvider.notifier).bump();
      setState(() {
        _draft = updated.rules;
        _version = updated.version;
        _dirty = false;
      });
      if (mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(const SnackBar(content: Text('已保存')));
      }
    } on ApiException catch (e) {
      if (mounted) {
        if (e.isVersionConflict) {
          ScaffoldMessenger.of(context).showSnackBar(const SnackBar(
              content: Text('规则已被其他端修改，正在刷新最新版本…')));
          ref.invalidate(rulesProvider);
          setState(() => _draft = null); // 重新拉取后 _load 覆盖草稿
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

  void _update(int index, AlertRule Function(AlertRule) edit) {
    setState(() {
      _draft = [
        for (var i = 0; i < _draft!.length; i++)
          i == index ? edit(_draft![i]) : _draft![i],
      ];
      _dirty = true;
    });
  }

  @override
  void dispose() {
    _heartbeatController.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final rulesAsync = ref.watch(rulesProvider);
    final rules = rulesAsync.value;
    if (rules != null) _load(rules);
    return Scaffold(
      appBar: AppBar(
        title: const Text('告警规则'),
        actions: [
          TextButton.icon(
            onPressed: (_saving || _draft == null || !_dirty) ? null : _save,
            icon: _saving
                ? const SizedBox(
                    width: 14,
                    height: 14,
                    child: CircularProgressIndicator(strokeWidth: 2))
                : const Icon(Icons.save_outlined, size: 18),
            label: const Text('保存'),
          ),
        ],
      ),
      body: AsyncValueView(
        value: rulesAsync,
        onRetry: () => ref.invalidate(rulesProvider),
        builder: (_) {
          final draft = _draft;
          if (draft == null) {
            return const Center(child: CircularProgressIndicator());
          }
          return ListView(
            padding: const EdgeInsets.only(bottom: 48),
            children: [
              Padding(
                padding: const EdgeInsets.fromLTRB(16, 12, 16, 0),
                child: Text(
                  '版本 v$_version${_dirty ? ' · 有未保存修改' : ''}',
                  style: Theme.of(context)
                      .textTheme
                      .bodySmall
                      ?.copyWith(color: Theme.of(context).colorScheme.outline),
                ),
              ),
              Card(
                margin: const EdgeInsets.fromLTRB(16, 12, 16, 0),
                child: Padding(
                  padding: const EdgeInsets.all(16),
                  child: Row(
                    children: [
                      Expanded(
                        child: TextFormField(
                          controller: _heartbeatController,
                          decoration: const InputDecoration(
                            labelText: '失联判定（秒）',
                            helperText: '连续无接入超过该时长判定心跳失联（60–3600）',
                          ),
                          keyboardType: TextInputType.number,
                          inputFormatters: [
                            FilteringTextInputFormatter.digitsOnly
                          ],
                          onChanged: (_) => setState(() => _dirty = true),
                        ),
                      ),
                    ],
                  ),
                ),
              ),
              const SectionTitle(text: '规则列表'),
              for (var i = 0; i < draft.length; i++)
                _RuleCard(
                  rule: draft[i],
                  onChanged: (edit) => _update(i, edit),
                ),
            ],
          );
        },
      ),
    );
  }
}

class _RuleCard extends StatelessWidget {
  const _RuleCard({required this.rule, required this.onChanged});

  final AlertRule rule;
  final void Function(AlertRule Function(AlertRule)) onChanged;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final scheme = theme.colorScheme;
    return Card(
      margin: const EdgeInsets.fromLTRB(16, 8, 16, 0),
      child: Padding(
        padding: const EdgeInsets.fromLTRB(16, 12, 16, 12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              children: [
                Expanded(
                  child: Text(rule.id, style: theme.textTheme.titleMedium),
                ),
                _SeveritySelector(
                  value: rule.severity,
                  onChanged: (v) => onChanged((r) => r.copyWith(severity: v)),
                ),
                Switch(
                  value: rule.enabled,
                  onChanged: (v) => onChanged((r) => r.copyWith(enabled: v)),
                ),
              ],
            ),
            Text(_kindLabel(rule), style: theme.textTheme.bodySmall?.copyWith(color: scheme.outline)),
            if (rule.kind == 'threshold') ...[
              const SizedBox(height: 12),
              _NumberRow(
                fields: [
                  ('阈值 %', rule.value, (v) => onChanged((r) => r.copyWith(value: v))),
                  ('持续 秒', rule.forSeconds?.toDouble(),
                      (v) => onChanged((r) => r.copyWith(forSeconds: v?.round()))),
                  ('恢复 %', rule.recoverValue,
                      (v) => onChanged((r) => r.copyWith(recoverValue: v))),
                  ('恢复持续 秒', rule.recoverForSeconds?.toDouble(),
                      (v) => onChanged(
                          (r) => r.copyWith(recoverForSeconds: v?.round()))),
                ],
              ),
            ],
          ],
        ),
      ),
    );
  }

  String _kindLabel(AlertRule r) {
    switch (r.kind) {
      case 'threshold':
        final metric = switch (r.metric) {
          'cpu_percent' => 'CPU',
          'mem_percent' => '内存',
          'disk_percent' => '磁盘',
          _ => r.metric ?? '?',
        };
        final label = r.label == null || r.label == '*' ? '全部' : r.label!;
        return '阈值：$metric（$label）超过阈值并持续超时 → 告警';
      case 'container_exit':
        return '容器退出：running → 非 running（match ${r.match ?? '*'}）';
      case 'smart':
        return 'SMART：磁盘判为 failing → 告警';
      case 'pool':
        return '存储池：state ≠ ok → 告警';
      default:
        return r.kind;
    }
  }
}

class _SeveritySelector extends StatelessWidget {
  const _SeveritySelector({required this.value, required this.onChanged});

  final String value;
  final ValueChanged<String> onChanged;

  @override
  Widget build(BuildContext context) {
    return DropdownButtonHideUnderline(
      child: DropdownButton<String>(
        value: value == 'critical' ? 'critical' : 'warning',
        items: const [
          DropdownMenuItem(value: 'warning', child: Text('警告')),
          DropdownMenuItem(value: 'critical', child: Text('严重')),
        ],
        onChanged: (v) {
          if (v != null) onChanged(v);
        },
      ),
    );
  }
}

class _NumberRow extends StatelessWidget {
  const _NumberRow({required this.fields});

  final List<(String, double?, void Function(double?))> fields;

  @override
  Widget build(BuildContext context) {
    return Wrap(
      spacing: 12,
      runSpacing: 8,
      children: [
        for (final (label, value, setter) in fields)
          SizedBox(
            width: 120,
            child: _NumberField(label: label, value: value, onChanged: setter),
          ),
      ],
    );
  }
}

class _NumberField extends StatefulWidget {
  const _NumberField({required this.label, required this.value, required this.onChanged});

  final String label;
  final double? value;
  final void Function(double?) onChanged;

  @override
  State<_NumberField> createState() => _NumberFieldState();
}

class _NumberFieldState extends State<_NumberField> {
  late final TextEditingController _controller;

  @override
  void initState() {
    super.initState();
    _controller = TextEditingController(
        text: widget.value == null
            ? ''
            : (widget.value! % 1 == 0
                ? widget.value!.toStringAsFixed(0)
                : '${widget.value}'));
  }

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return TextField(
      controller: _controller,
      decoration: InputDecoration(
        labelText: widget.label,
        isDense: true,
      ),
      keyboardType: const TextInputType.numberWithOptions(decimal: true),
      inputFormatters: [
        FilteringTextInputFormatter.allow(RegExp(r'[\d.]')),
      ],
      onChanged: (v) => widget.onChanged(double.tryParse(v)),
    );
  }
}
