import 'package:flutter/material.dart';

import '../models/fault.dart';
import '../models/message.dart';
import '../theme/app_theme.dart';
import 'common.dart';
import 'format.dart';

/// 首页未恢复故障条目：severity 色阶、静音标记、轮次、事件数。
class FaultTile extends StatelessWidget {
  const FaultTile({super.key, required this.fault, this.onTap});

  final Fault fault;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    final color = SeverityColors.of(fault.severity, scheme);
    return ListTile(
      onTap: onTap,
      leading: Container(
        width: 40,
        height: 40,
        decoration: BoxDecoration(
          color: color.withValues(alpha: 0.12),
          borderRadius: BorderRadius.circular(10),
        ),
        child: Icon(
          fault.isOpen ? Icons.warning_amber_rounded : Icons.check_circle_outline,
          color: color,
          size: 22,
        ),
      ),
      title: Row(
        children: [
          Expanded(
            child: Text(
              fault.title,
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: TextStyle(
                fontWeight: fault.isRead ? FontWeight.normal : FontWeight.w600,
              ),
            ),
          ),
          if (fault.muted)
            Padding(
              padding: const EdgeInsets.only(left: 4),
              child: Icon(Icons.notifications_off_outlined,
                  size: 16, color: scheme.outline),
            ),
        ],
      ),
      subtitle: Text(
        '${fault.sourceName} · 第 ${fault.incident} 轮 · ${fault.eventCount} 条事件 · ${formatRelative(fault.lastEventAt)}',
        maxLines: 1,
        overflow: TextOverflow.ellipsis,
        style: Theme.of(context).textTheme.bodySmall,
      ),
      trailing: SeverityBadge(severity: fault.severity),
    );
  }
}

/// 历史/消息列表条目。
class MessageTile extends StatelessWidget {
  const MessageTile({super.key, required this.message, this.onTap});

  final Message message;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    final scheme = Theme.of(context).colorScheme;
    return ListTile(
      onTap: onTap,
      leading: Padding(
        padding: const EdgeInsets.only(top: 6),
        child: SeverityDot(severity: message.severity),
      ),
      title: Text(
        message.title,
        maxLines: 1,
        overflow: TextOverflow.ellipsis,
        style: TextStyle(
          fontWeight: message.isRead ? FontWeight.normal : FontWeight.w600,
        ),
      ),
      subtitle: Text(
        '${message.sourceName} · ${kindLabel(message.kind)} · ${formatRelative(message.receivedAt)}',
        maxLines: 1,
        overflow: TextOverflow.ellipsis,
        style: Theme.of(context).textTheme.bodySmall,
      ),
      trailing: Column(
        mainAxisAlignment: MainAxisAlignment.center,
        crossAxisAlignment: CrossAxisAlignment.end,
        children: [
          if (!message.isRead)
            Container(
              width: 8,
              height: 8,
              decoration:
                  BoxDecoration(color: scheme.primary, shape: BoxShape.circle),
            ),
          if (message.belongsToFault)
            Padding(
              padding: const EdgeInsets.only(top: 4),
              child: Icon(Icons.link, size: 14, color: scheme.outline),
            ),
        ],
      ),
    );
  }
}
