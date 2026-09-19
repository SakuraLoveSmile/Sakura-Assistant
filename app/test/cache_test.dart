import 'package:assistant/cache/assistant_cache.dart';
import 'package:assistant/models/sync.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sqflite_common_ffi/sqflite_ffi.dart';

void main() {
  late AssistantCache cache;

  setUpAll(() async {
    sqfliteFfiInit();
    cache = await AssistantCache.inMemory(databaseFactoryFfi);
  });

  test('sync 应用 → tombstone 删除（source/message/fault 全链路）', () async {
    await cache.applyChanges([
      const SyncChange(changeSeq: 1, type: 'source', data: {
        'id': 'src1',
        'name': 'nas',
        'kind': 'device',
      }),
      const SyncChange(changeSeq: 2, type: 'message', data: {
        'id': 'm1',
        'title': 'hello',
      }),
      const SyncChange(changeSeq: 3, type: 'fault', data: {
        'id': 'f1',
        'faultKey': 'cpu',
        'state': 'open',
      }),
    ]);
    expect(await cache.sources(), hasLength(1));
    expect(await cache.messages(), hasLength(1));
    expect(await cache.faults(), hasLength(1));

    await cache.applyChanges([
      const SyncChange(changeSeq: 4, type: 'tombstone', data: {
        'type': 'source',
        'id': 'src1',
      }),
      const SyncChange(changeSeq: 5, type: 'tombstone', data: {
        'type': 'message',
        'id': 'm1',
      }),
      const SyncChange(changeSeq: 6, type: 'tombstone', data: {
        'type': 'fault',
        'id': 'f1',
      }),
    ]);
    expect(await cache.sources(), isEmpty);
    expect(await cache.messages(), isEmpty);
    expect(await cache.faults(), isEmpty);
  });

  test('applySyncPage 推进 cursor；未知变更类型忽略不崩', () async {
    await cache.applySyncPage(const SyncPage(
      cursor: 42,
      hasMore: false,
      changes: [
        SyncChange(changeSeq: 7, type: 'future_type', data: {'x': 1}),
      ],
    ));
    expect(await cache.cursor, 42);
  });
}
