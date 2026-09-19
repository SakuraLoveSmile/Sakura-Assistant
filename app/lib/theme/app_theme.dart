import 'package:flutter/material.dart';

/// Material 3 双主题（跟随系统明暗）。
class AppTheme {
  AppTheme._();

  static const seed = Color(0xFF3D6FE0);

  static ThemeData light() => _base(ColorScheme.fromSeed(
        seedColor: seed,
        brightness: Brightness.light,
      ));

  static ThemeData dark() => _base(ColorScheme.fromSeed(
        seedColor: seed,
        brightness: Brightness.dark,
      ));

  static ThemeData _base(ColorScheme scheme) {
    return ThemeData(
      useMaterial3: true,
      colorScheme: scheme,
      visualDensity: VisualDensity.adaptivePlatformDensity,
      cardTheme: CardThemeData(
        elevation: 0,
        shape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(16),
          side: BorderSide(color: scheme.outlineVariant.withValues(alpha: 0.5)),
        ),
        clipBehavior: Clip.antiAlias,
        margin: EdgeInsets.zero,
      ),
      appBarTheme: AppBarTheme(
        centerTitle: false,
        elevation: 0,
        scrolledUnderElevation: 0.5,
        backgroundColor: scheme.surface,
      ),
      listTileTheme: const ListTileThemeData(
        contentPadding: EdgeInsets.symmetric(horizontal: 16),
      ),
      inputDecorationTheme: InputDecorationTheme(
        border: OutlineInputBorder(borderRadius: BorderRadius.circular(12)),
        filled: true,
      ),
      snackBarTheme: const SnackBarThemeData(behavior: SnackBarBehavior.floating),
    );
  }
}

/// 严重度配色（info / warning / critical + 未知）。
class SeverityColors {
  SeverityColors._();

  static Color of(String severity, ColorScheme scheme) {
    switch (severity) {
      case 'critical':
        return scheme.error;
      case 'warning':
        return const Color(0xFFE8960C);
      case 'info':
      default:
        return scheme.primary;
    }
  }

  static Color containerOf(String severity, ColorScheme scheme) {
    switch (severity) {
      case 'critical':
        return scheme.errorContainer;
      case 'warning':
        return const Color(0xFFFFF3D6);
      case 'info':
      default:
        return scheme.primaryContainer;
    }
  }
}
