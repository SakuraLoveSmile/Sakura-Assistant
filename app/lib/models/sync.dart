import 'json.dart';

/// `/api/v1/sync` 的单条变更。`data` 的具体类型由 [type] 决定：
/// message | fault | source | rules | settings | tombstone。
class SyncChange {
  const SyncChange({
    required this.changeSeq,
    required this.type,
    required this.data,
  });

  final int changeSeq;
  final String type;
  final Map<String, dynamic> data;

  factory SyncChange.fromJson(Map<String, dynamic> json) => SyncChange(
        changeSeq: asInt(json['changeSeq']),
        type: asString(json['type']),
        data: asMap(json['data']),
      );
}

/// `GET /api/v1/sync` 一页响应。
class SyncPage {
  const SyncPage({
    required this.cursor,
    required this.hasMore,
    required this.changes,
  });

  final int cursor;
  final bool hasMore;
  final List<SyncChange> changes;

  factory SyncPage.fromJson(Map<String, dynamic> json) => SyncPage(
        cursor: asInt(json['cursor']),
        hasMore: asBool(json['hasMore']),
        changes: asMapList(json['changes']).map(SyncChange.fromJson).toList(),
      );
}
