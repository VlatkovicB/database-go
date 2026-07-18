package api

import (
	"database/internal/executor"
	"database/internal/lexer"
	"database/internal/parser"
	"database/internal/storage"
	"fmt"
	"net/http"
)

var seedStatements = []string{
	// DROP
	"DROP TABLE battles",
	"DROP TABLE quest_progress",
	"DROP TABLE quests",
	"DROP TABLE character_skills",
	"DROP TABLE skills",
	"DROP TABLE inventory",
	"DROP TABLE items",
	"DROP TABLE characters",
	"DROP TABLE guild_members",
	"DROP TABLE guilds",
	"DROP TABLE players",

	// SCHEMA
	"CREATE TABLE players (id INT PRIMARY KEY, username TEXT, level INT, xp INT, gold INT, class TEXT)",
	"CREATE TABLE guilds (id INT PRIMARY KEY, name TEXT, tag TEXT, level INT, gold INT, prestige INT)",
	"CREATE TABLE guild_members (id INT PRIMARY KEY, guild_id INT, player_id INT, rank TEXT, contribution INT)",
	"CREATE TABLE characters (id INT PRIMARY KEY, player_id INT, guild_id INT, name TEXT, class TEXT, level INT, hp INT, mana INT, strength INT, agility INT)",
	"CREATE TABLE items (id INT PRIMARY KEY, name TEXT, type TEXT, rarity TEXT, damage INT, armor INT, value INT, level_req INT)",
	"CREATE TABLE inventory (id INT PRIMARY KEY, character_id INT, item_id INT, quantity INT, equipped INT)",
	"CREATE TABLE skills (id INT PRIMARY KEY, name TEXT, type TEXT, damage INT, mana_cost INT, cooldown INT)",
	"CREATE TABLE character_skills (id INT PRIMARY KEY, character_id INT, skill_id INT, skill_level INT, uses INT)",
	"CREATE TABLE quests (id INT PRIMARY KEY, name TEXT, difficulty TEXT, min_level INT, xp_reward INT, gold_reward INT, repeatable INT)",
	"CREATE TABLE quest_progress (id INT PRIMARY KEY, character_id INT, quest_id INT, status TEXT, progress INT, attempts INT)",
	"CREATE TABLE battles (id INT PRIMARY KEY, attacker_id INT, defender_id INT, winner_id INT, damage_dealt INT, rounds INT, arena TEXT)",

	// PLAYERS
	"INSERT INTO players VALUES (1,  'ShadowBlade',  42, 184200, 9400,  'Rogue')",
	"INSERT INTO players VALUES (2,  'IronFist',     38, 142800, 6200,  'Warrior')",
	"INSERT INTO players VALUES (3,  'ArcaneWitch',  55, 310500, 18900, 'Mage')",
	"INSERT INTO players VALUES (4,  'StormRider',   29,  74100, 3100,  'Archer')",
	"INSERT INTO players VALUES (5,  'NightHunter',  61, 420000, 24700, 'Rogue')",
	"INSERT INTO players VALUES (6,  'BlazeKnight',  47, 218400, 11200, 'Warrior')",
	"INSERT INTO players VALUES (7,  'FrostSeer',    33,  98200, 4400,  'Mage')",
	"INSERT INTO players VALUES (8,  'VoidWalker',   70, 680000, 41000, 'Rogue')",
	"INSERT INTO players VALUES (9,  'TidalForce',   25,  48000, 1900,  'Warrior')",
	"INSERT INTO players VALUES (10, 'GoldenArrow',  58, 364000, 21400, 'Archer')",
	"INSERT INTO players VALUES (11, 'CrimsonMage',  44, 196000, 10100, 'Mage')",
	"INSERT INTO players VALUES (12, 'ThunderAxe',   36, 128000, 5500,  'Warrior')",
	"INSERT INTO players VALUES (13, 'SilentArrow',  52, 278000, 16200, 'Archer')",
	"INSERT INTO players VALUES (14, 'MoonDancer',   19,  22000,  800,  'Rogue')",
	"INSERT INTO players VALUES (15, 'EarthShaker',  63, 455000, 28000, 'Warrior')",
	"INSERT INTO players VALUES (16, 'StarWeaver',   41, 172000, 8700,  'Mage')",
	"INSERT INTO players VALUES (17, 'RuneScribe',   50, 254000, 14900, 'Mage')",
	"INSERT INTO players VALUES (18, 'SwiftBlade',   27,  61000, 2400,  'Rogue')",
	"INSERT INTO players VALUES (19, 'IronClad',     66, 520000, 33000, 'Warrior')",
	"INSERT INTO players VALUES (20, 'BoneWarden',   31,  88000, 3800,  'Warrior')",

	// GUILDS
	"INSERT INTO guilds VALUES (1, 'Eternal Vanguard', 'EVG', 15, 840000, 9)",
	"INSERT INTO guilds VALUES (2, 'Shadow Council',   'SC',  12, 520000, 7)",
	"INSERT INTO guilds VALUES (3, 'Iron Brotherhood', 'IB',   8, 310000, 4)",
	"INSERT INTO guilds VALUES (4, 'Arcane Order',     'AO',  11, 480000, 6)",
	"INSERT INTO guilds VALUES (5, 'Lone Wolves',      'LW',   3,  95000, 1)",

	// GUILD MEMBERS
	"INSERT INTO guild_members VALUES (1,  1, 1,  'Officer', 48200)",
	"INSERT INTO guild_members VALUES (2,  1, 2,  'Member',  31400)",
	"INSERT INTO guild_members VALUES (3,  1, 6,  'Officer', 52100)",
	"INSERT INTO guild_members VALUES (4,  1, 15, 'Leader',  98000)",
	"INSERT INTO guild_members VALUES (5,  1, 19, 'Member',  61000)",
	"INSERT INTO guild_members VALUES (6,  2, 3,  'Leader',  87500)",
	"INSERT INTO guild_members VALUES (7,  2, 5,  'Officer', 74200)",
	"INSERT INTO guild_members VALUES (8,  2, 8,  'Member',  41000)",
	"INSERT INTO guild_members VALUES (9,  2, 13, 'Member',  38900)",
	"INSERT INTO guild_members VALUES (10, 3, 4,  'Member',  14200)",
	"INSERT INTO guild_members VALUES (11, 3, 12, 'Leader',  42000)",
	"INSERT INTO guild_members VALUES (12, 3, 20, 'Member',  18700)",
	"INSERT INTO guild_members VALUES (13, 4, 7,  'Member',  22100)",
	"INSERT INTO guild_members VALUES (14, 4, 11, 'Officer', 44800)",
	"INSERT INTO guild_members VALUES (15, 4, 16, 'Member',  31200)",
	"INSERT INTO guild_members VALUES (16, 4, 17, 'Leader',  68000)",
	"INSERT INTO guild_members VALUES (17, 5, 9,  'Member',   8100)",
	"INSERT INTO guild_members VALUES (18, 5, 14, 'Member',   4200)",
	"INSERT INTO guild_members VALUES (19, 5, 18, 'Leader',  12500)",

	// CHARACTERS
	"INSERT INTO characters VALUES (1,  1,  1, 'Kael',      'Rogue',   42, 980,  200, 55, 88)",
	"INSERT INTO characters VALUES (2,  2,  1, 'Theron',    'Warrior', 38, 1800, 100, 82, 41)",
	"INSERT INTO characters VALUES (3,  3,  2, 'Lyria',     'Mage',    55, 700,  1400, 30, 62)",
	"INSERT INTO characters VALUES (4,  4,  3, 'Brom',      'Paladin', 29, 1400, 600,  68, 48)",
	"INSERT INTO characters VALUES (5,  5,  2, 'Vesper',    'Archer',  61, 1100, 300,  44, 95)",
	"INSERT INTO characters VALUES (6,  6,  1, 'Dario',     'Warrior', 47, 1950, 120,  88, 38)",
	"INSERT INTO characters VALUES (7,  7,  4, 'Selene',    'Mage',    33, 650,  1100, 28, 58)",
	"INSERT INTO characters VALUES (8,  8,  2, 'Zeth',      'Rogue',   70, 1300, 280,  72, 99)",
	"INSERT INTO characters VALUES (9,  9,  5, 'Goran',     'Warrior', 25, 1550, 80,   74, 33)",
	"INSERT INTO characters VALUES (10, 10, 1, 'Petra',     'Archer',  58, 1050, 260,  40, 92)",
	"INSERT INTO characters VALUES (11, 11, 4, 'Caius',     'Mage',    44, 720,  1250, 32, 60)",
	"INSERT INTO characters VALUES (12, 12, 3, 'Wren',      'Warrior', 36, 1700, 90,   80, 39)",
	"INSERT INTO characters VALUES (13, 13, 2, 'Raina',     'Archer',  52, 1000, 240,  38, 90)",
	"INSERT INTO characters VALUES (14, 14, 5, 'Pip',       'Rogue',   19, 700,  150,  40, 72)",
	"INSERT INTO characters VALUES (15, 15, 1, 'Magnus',    'Warrior', 63, 2100, 110,  95, 36)",
	"INSERT INTO characters VALUES (16, 16, 4, 'Aria',      'Mage',    41, 680,  1180, 29, 59)",
	"INSERT INTO characters VALUES (17, 17, 4, 'Dorian',    'Mage',    50, 710,  1350, 31, 61)",
	"INSERT INTO characters VALUES (18, 18, 5, 'Lira',      'Rogue',   27, 820,  160,  46, 80)",
	"INSERT INTO characters VALUES (19, 19, 1, 'Aldric',    'Warrior', 66, 2200, 100,  98, 34)",
	"INSERT INTO characters VALUES (20, 20, 3, 'Crag',      'Warrior', 31, 1620, 85,   77, 37)",
	"INSERT INTO characters VALUES (21, 1,  1, 'Kael-II',   'Archer',  18, 820,  180,  35, 70)",
	"INSERT INTO characters VALUES (22, 3,  2, 'Lyria-Alt', 'Rogue',   22, 860,  140,  48, 76)",
	"INSERT INTO characters VALUES (23, 5,  2, 'Vesper-II', 'Mage',    15, 580,  900,  24, 52)",
	"INSERT INTO characters VALUES (24, 8,  2, 'Zeth-Alt',  'Warrior', 55, 1900, 95,   90, 40)",
	"INSERT INTO characters VALUES (25, 10, 1, 'Petra-II',  'Mage',    30, 640,  1050, 27, 55)",
	"INSERT INTO characters VALUES (26, 15, 1, 'Magnus-II', 'Rogue',   48, 1050, 220,  60, 85)",
	"INSERT INTO characters VALUES (27, 19, 1, 'Aldric-II', 'Paladin', 44, 1500, 650,  72, 50)",
	"INSERT INTO characters VALUES (28, 2,  1, 'Theron-II', 'Archer',  35, 980,  200,  38, 82)",

	// ITEMS
	"INSERT INTO items VALUES (1,  'Shadow Dagger',       'weapon',    'rare',      85,  0,   1200, 30)",
	"INSERT INTO items VALUES (2,  'Plate of the Titan',  'armor',     'epic',       0,  80,  4500, 45)",
	"INSERT INTO items VALUES (3,  'Staff of Eternity',   'weapon',    'legendary', 220, 0,  12000, 55)",
	"INSERT INTO items VALUES (4,  'Iron Shield',         'armor',     'common',     0,  25,   350, 10)",
	"INSERT INTO items VALUES (5,  'Health Potion',       'consumable','common',     0,  0,     50,  1)",
	"INSERT INTO items VALUES (6,  'Elven Bow',           'weapon',    'rare',      110, 0,   2200, 35)",
	"INSERT INTO items VALUES (7,  'Mana Crystal',        'consumable','uncommon',   0,  0,    180,  1)",
	"INSERT INTO items VALUES (8,  'Dragon Scale Helm',   'armor',     'epic',       0,  55,  5800, 50)",
	"INSERT INTO items VALUES (9,  'Void Blade',          'weapon',    'legendary', 310, 0,  18000, 65)",
	"INSERT INTO items VALUES (10, 'Leather Boots',       'armor',     'common',     0,  10,   120, 5)",
	"INSERT INTO items VALUES (11, 'Magic Ring',          'accessory', 'rare',       20, 5,   950, 25)",
	"INSERT INTO items VALUES (12, 'Crossbow of Ruin',    'weapon',    'epic',      180, 0,   7200, 48)",
	"INSERT INTO items VALUES (13, 'Mithril Chestplate',  'armor',     'rare',       0,  65,  3100, 38)",
	"INSERT INTO items VALUES (14, 'Rune Sword',          'weapon',    'epic',      165, 0,   6800, 42)",
	"INSERT INTO items VALUES (15, 'Elixir of Power',     'consumable','rare',       0,  0,    420,  1)",
	"INSERT INTO items VALUES (16, 'Cloak of Shadows',    'armor',     'rare',       0,  18,   880, 28)",
	"INSERT INTO items VALUES (17, 'Thunder Amulet',      'accessory', 'epic',       45, 15,  4100, 44)",
	"INSERT INTO items VALUES (18, 'Beginner Sword',      'weapon',    'common',     22, 0,    80,  1)",
	"INSERT INTO items VALUES (19, 'Wizard Hat',          'armor',     'uncommon',   0,  12,   310, 15)",
	"INSERT INTO items VALUES (20, 'Phoenix Feather',     'consumable','legendary',  0,  0,   2800,  1)",

	// INVENTORY
	"INSERT INTO inventory VALUES (1,  1,  1,  1, 1)",
	"INSERT INTO inventory VALUES (2,  1,  5,  12, 0)",
	"INSERT INTO inventory VALUES (3,  1,  7,  5, 0)",
	"INSERT INTO inventory VALUES (4,  1,  16, 1, 1)",
	"INSERT INTO inventory VALUES (5,  2,  2,  1, 1)",
	"INSERT INTO inventory VALUES (6,  2,  4,  1, 1)",
	"INSERT INTO inventory VALUES (7,  2,  5,  8, 0)",
	"INSERT INTO inventory VALUES (8,  2,  15, 2, 0)",
	"INSERT INTO inventory VALUES (9,  3,  3,  1, 1)",
	"INSERT INTO inventory VALUES (10, 3,  7,  20, 0)",
	"INSERT INTO inventory VALUES (11, 3,  19, 1, 1)",
	"INSERT INTO inventory VALUES (12, 4,  4,  1, 1)",
	"INSERT INTO inventory VALUES (13, 4,  5,  15, 0)",
	"INSERT INTO inventory VALUES (14, 4,  17, 1, 1)",
	"INSERT INTO inventory VALUES (15, 5,  6,  1, 1)",
	"INSERT INTO inventory VALUES (16, 5,  8,  1, 1)",
	"INSERT INTO inventory VALUES (17, 5,  5,  6, 0)",
	"INSERT INTO inventory VALUES (18, 6,  2,  1, 1)",
	"INSERT INTO inventory VALUES (19, 6,  14, 1, 1)",
	"INSERT INTO inventory VALUES (20, 7,  3,  1, 0)",
	"INSERT INTO inventory VALUES (21, 7,  7,  8, 0)",
	"INSERT INTO inventory VALUES (22, 8,  9,  1, 1)",
	"INSERT INTO inventory VALUES (23, 8,  16, 1, 1)",
	"INSERT INTO inventory VALUES (24, 8,  5,  3, 0)",
	"INSERT INTO inventory VALUES (25, 8,  20, 1, 0)",
	"INSERT INTO inventory VALUES (26, 10, 12, 1, 1)",
	"INSERT INTO inventory VALUES (27, 10, 13, 1, 1)",
	"INSERT INTO inventory VALUES (28, 11, 3,  1, 1)",
	"INSERT INTO inventory VALUES (29, 11, 17, 1, 1)",
	"INSERT INTO inventory VALUES (30, 13, 6,  1, 1)",
	"INSERT INTO inventory VALUES (31, 13, 8,  1, 1)",
	"INSERT INTO inventory VALUES (32, 15, 2,  1, 1)",
	"INSERT INTO inventory VALUES (33, 15, 9,  1, 1)",
	"INSERT INTO inventory VALUES (34, 15, 20, 2, 0)",
	"INSERT INTO inventory VALUES (35, 19, 9,  1, 1)",
	"INSERT INTO inventory VALUES (36, 19, 2,  1, 1)",
	"INSERT INTO inventory VALUES (37, 9,  18, 1, 1)",
	"INSERT INTO inventory VALUES (38, 14, 18, 1, 1)",
	"INSERT INTO inventory VALUES (39, 14, 5,  4, 0)",
	"INSERT INTO inventory VALUES (40, 20, 4,  1, 1)",

	// SKILLS
	"INSERT INTO skills VALUES (1,  'Shadowstrike',   'physical', 145, 40,  3)",
	"INSERT INTO skills VALUES (2,  'Whirlwind',      'physical', 190, 60,  5)",
	"INSERT INTO skills VALUES (3,  'Fireball',       'magic',    280, 120, 4)",
	"INSERT INTO skills VALUES (4,  'Blizzard',       'magic',    340, 180, 8)",
	"INSERT INTO skills VALUES (5,  'Holy Shield',    'defense',    0, 80,  6)",
	"INSERT INTO skills VALUES (6,  'Arrow Rain',     'physical', 210, 70,  5)",
	"INSERT INTO skills VALUES (7,  'Poison Blade',   'physical',  95, 30,  2)",
	"INSERT INTO skills VALUES (8,  'Arcane Burst',   'magic',    390, 220, 10)",
	"INSERT INTO skills VALUES (9,  'Battle Cry',     'defense',    0, 50,  4)",
	"INSERT INTO skills VALUES (10, 'Shadow Clone',   'physical', 120, 90,  7)",

	// CHARACTER SKILLS
	"INSERT INTO character_skills VALUES (1,  1,  1,  5, 842)",
	"INSERT INTO character_skills VALUES (2,  1,  7,  3, 1240)",
	"INSERT INTO character_skills VALUES (3,  1,  10, 2, 311)",
	"INSERT INTO character_skills VALUES (4,  2,  2,  4, 520)",
	"INSERT INTO character_skills VALUES (5,  2,  9,  3, 780)",
	"INSERT INTO character_skills VALUES (6,  3,  3,  8, 1920)",
	"INSERT INTO character_skills VALUES (7,  3,  4,  6, 880)",
	"INSERT INTO character_skills VALUES (8,  3,  8,  7, 440)",
	"INSERT INTO character_skills VALUES (9,  4,  5,  5, 670)",
	"INSERT INTO character_skills VALUES (10, 4,  9,  4, 910)",
	"INSERT INTO character_skills VALUES (11, 5,  6,  9, 2100)",
	"INSERT INTO character_skills VALUES (12, 5,  1,  4, 390)",
	"INSERT INTO character_skills VALUES (13, 6,  2,  6, 1100)",
	"INSERT INTO character_skills VALUES (14, 6,  9,  5, 860)",
	"INSERT INTO character_skills VALUES (15, 7,  3,  4, 720)",
	"INSERT INTO character_skills VALUES (16, 7,  8,  3, 200)",
	"INSERT INTO character_skills VALUES (17, 8,  1,  9, 3400)",
	"INSERT INTO character_skills VALUES (18, 8,  7,  8, 2800)",
	"INSERT INTO character_skills VALUES (19, 8,  10, 7, 1900)",
	"INSERT INTO character_skills VALUES (20, 10, 6,  7, 1600)",
	"INSERT INTO character_skills VALUES (21, 11, 3,  6, 1100)",
	"INSERT INTO character_skills VALUES (22, 11, 8,  5, 680)",
	"INSERT INTO character_skills VALUES (23, 13, 6,  8, 2200)",
	"INSERT INTO character_skills VALUES (24, 15, 2,  9, 2400)",
	"INSERT INTO character_skills VALUES (25, 15, 9,  8, 1800)",
	"INSERT INTO character_skills VALUES (26, 17, 3,  7, 1400)",
	"INSERT INTO character_skills VALUES (27, 17, 4,  5, 640)",
	"INSERT INTO character_skills VALUES (28, 19, 2,  9, 2600)",
	"INSERT INTO character_skills VALUES (29, 19, 9,  9, 2100)",
	"INSERT INTO character_skills VALUES (30, 26, 1,  6, 1050)",

	// QUESTS
	"INSERT INTO quests VALUES (1,  'Goblin Purge',           'easy',      5,  500,   200, 1)",
	"INSERT INTO quests VALUES (2,  'The Dark Crypt',         'hard',     35, 2400,  1800, 0)",
	"INSERT INTO quests VALUES (3,  'Dragon Hunt',            'legendary',55,12000,  9500, 0)",
	"INSERT INTO quests VALUES (4,  'Bandit Ambush',          'medium',   15,  900,   600, 1)",
	"INSERT INTO quests VALUES (5,  'Escort the Merchant',    'easy',      1,  350,   450, 1)",
	"INSERT INTO quests VALUES (6,  'Tower of Madness',       'hard',     40, 3200,  2400, 0)",
	"INSERT INTO quests VALUES (7,  'Cursed Forest',          'medium',   20, 1100,   800, 0)",
	"INSERT INTO quests VALUES (8,  'Arena Challenge',        'medium',   10,  800,   700, 1)",
	"INSERT INTO quests VALUES (9,  'Ancient Ruins',          'hard',     45, 2800,  2100, 0)",
	"INSERT INTO quests VALUES (10, 'Sea Serpent Slayer',     'hard',     50, 3600,  2700, 0)",
	"INSERT INTO quests VALUES (11, 'Dungeon Crawler',        'medium',   25, 1300,   950, 1)",
	"INSERT INTO quests VALUES (12, 'Void Rift',              'legendary',60,15000, 11000, 0)",
	"INSERT INTO quests VALUES (13, 'Guild War Offensive',    'hard',     38, 2600,  1900, 0)",
	"INSERT INTO quests VALUES (14, 'Herb Gathering',         'easy',      1,  200,   300, 1)",
	"INSERT INTO quests VALUES (15, 'Shadow Assassination',   'hard',     45, 3000,  2200, 0)",

	// QUEST PROGRESS
	"INSERT INTO quest_progress VALUES (1,  1,  1,  'completed',   100, 3)",
	"INSERT INTO quest_progress VALUES (2,  1,  2,  'in_progress',  60, 1)",
	"INSERT INTO quest_progress VALUES (3,  1,  3,  'in_progress',  15, 1)",
	"INSERT INTO quest_progress VALUES (4,  1,  4,  'completed',   100, 2)",
	"INSERT INTO quest_progress VALUES (5,  1,  8,  'completed',   100, 5)",
	"INSERT INTO quest_progress VALUES (6,  1,  15, 'in_progress',  40, 1)",
	"INSERT INTO quest_progress VALUES (7,  2,  1,  'completed',   100, 4)",
	"INSERT INTO quest_progress VALUES (8,  2,  4,  'completed',   100, 6)",
	"INSERT INTO quest_progress VALUES (9,  2,  5,  'completed',   100, 8)",
	"INSERT INTO quest_progress VALUES (10, 2,  8,  'completed',   100, 3)",
	"INSERT INTO quest_progress VALUES (11, 2,  11, 'completed',   100, 2)",
	"INSERT INTO quest_progress VALUES (12, 2,  13, 'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (13, 3,  3,  'in_progress',  45, 2)",
	"INSERT INTO quest_progress VALUES (14, 3,  6,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (15, 3,  9,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (16, 3,  10, 'in_progress',  70, 1)",
	"INSERT INTO quest_progress VALUES (17, 3,  12, 'in_progress',  30, 1)",
	"INSERT INTO quest_progress VALUES (18, 4,  1,  'completed',   100, 2)",
	"INSERT INTO quest_progress VALUES (19, 4,  5,  'completed',   100, 4)",
	"INSERT INTO quest_progress VALUES (20, 4,  7,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (21, 5,  2,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (22, 5,  3,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (23, 5,  6,  'completed',   100, 2)",
	"INSERT INTO quest_progress VALUES (24, 5,  9,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (25, 5,  12, 'in_progress',  55, 1)",
	"INSERT INTO quest_progress VALUES (26, 6,  2,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (27, 6,  6,  'in_progress',  80, 1)",
	"INSERT INTO quest_progress VALUES (28, 7,  1,  'completed',   100, 6)",
	"INSERT INTO quest_progress VALUES (29, 7,  4,  'completed',   100, 3)",
	"INSERT INTO quest_progress VALUES (30, 7,  7,  'in_progress',  50, 1)",
	"INSERT INTO quest_progress VALUES (31, 8,  3,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (32, 8,  12, 'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (33, 8,  15, 'in_progress',  65, 1)",
	"INSERT INTO quest_progress VALUES (34, 10, 3,  'in_progress',  25, 1)",
	"INSERT INTO quest_progress VALUES (35, 10, 9,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (36, 10, 10, 'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (37, 11, 6,  'in_progress',  90, 2)",
	"INSERT INTO quest_progress VALUES (38, 11, 9,  'in_progress',  55, 1)",
	"INSERT INTO quest_progress VALUES (39, 13, 9,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (40, 13, 10, 'in_progress',  80, 1)",
	"INSERT INTO quest_progress VALUES (41, 15, 3,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (42, 15, 6,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (43, 15, 12, 'in_progress',  40, 1)",
	"INSERT INTO quest_progress VALUES (44, 17, 6,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (45, 17, 9,  'in_progress',  60, 1)",
	"INSERT INTO quest_progress VALUES (46, 19, 3,  'completed',   100, 2)",
	"INSERT INTO quest_progress VALUES (47, 19, 12, 'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (48, 9,  1,  'completed',   100, 1)",
	"INSERT INTO quest_progress VALUES (49, 9,  5,  'completed',   100, 2)",
	"INSERT INTO quest_progress VALUES (50, 14, 5,  'in_progress',  40, 1)",

	// BATTLES
	"INSERT INTO battles VALUES (1,  1,  2,  1,  430, 8,  'Forest Arena')",
	"INSERT INTO battles VALUES (2,  3,  5,  3,  890, 12, 'City Arena')",
	"INSERT INTO battles VALUES (3,  2,  4,  4,  310, 6,  'Dungeon Pit')",
	"INSERT INTO battles VALUES (4,  5,  1,  5,  620, 10, 'Forest Arena')",
	"INSERT INTO battles VALUES (5,  3,  1,  3,  750, 14, 'City Arena')",
	"INSERT INTO battles VALUES (6,  4,  3,  3,  280, 5,  'Dungeon Pit')",
	"INSERT INTO battles VALUES (7,  1,  5,  1,  510, 9,  'Forest Arena')",
	"INSERT INTO battles VALUES (8,  8,  3,  8, 1240, 7,  'Shadow Realm')",
	"INSERT INTO battles VALUES (9,  8,  5,  8,  980, 11, 'Shadow Realm')",
	"INSERT INTO battles VALUES (10, 15, 8,  8,  870, 15, 'City Arena')",
	"INSERT INTO battles VALUES (11, 19, 8,  19, 730, 13, 'Dungeon Pit')",
	"INSERT INTO battles VALUES (12, 8,  19, 8,  900, 16, 'Shadow Realm')",
	"INSERT INTO battles VALUES (13, 6,  2,  6,  580, 7,  'Forest Arena')",
	"INSERT INTO battles VALUES (14, 10, 13, 10, 640, 9,  'City Arena')",
	"INSERT INTO battles VALUES (15, 11, 7,  11, 770, 11, 'Arcane Sanctum')",
	"INSERT INTO battles VALUES (16, 3,  11, 3,  920, 13, 'Arcane Sanctum')",
	"INSERT INTO battles VALUES (17, 17, 7,  17, 810, 10, 'Arcane Sanctum')",
	"INSERT INTO battles VALUES (18, 5,  10, 5,  710, 8,  'Forest Arena')",
	"INSERT INTO battles VALUES (19, 1,  4,  1,  480, 7,  'Dungeon Pit')",
	"INSERT INTO battles VALUES (20, 2,  9,  2,  390, 5,  'Iron Keep')",
	"INSERT INTO battles VALUES (21, 15, 19, 15, 650, 12, 'City Arena')",
	"INSERT INTO battles VALUES (22, 8,  15, 8, 1100, 18, 'Shadow Realm')",
	"INSERT INTO battles VALUES (23, 5,  8,  8,  950, 14, 'Shadow Realm')",
	"INSERT INTO battles VALUES (24, 6,  15, 15, 720, 9,  'Iron Keep')",
	"INSERT INTO battles VALUES (25, 3,  17, 3,  840, 11, 'Arcane Sanctum')",
	"INSERT INTO battles VALUES (26, 13, 10, 13, 590, 8,  'Forest Arena')",
	"INSERT INTO battles VALUES (27, 1,  6,  6,  470, 10, 'Iron Keep')",
	"INSERT INTO battles VALUES (28, 19, 6,  19, 610, 7,  'Dungeon Pit')",
	"INSERT INTO battles VALUES (29, 4,  12, 12, 340, 6,  'Iron Keep')",
	"INSERT INTO battles VALUES (30, 9,  4,  4,  260, 4,  'Dungeon Pit')",
	"INSERT INTO battles VALUES (31, 14, 9,  9,  210, 5,  'Forest Arena')",
	"INSERT INTO battles VALUES (32, 7,  16, 16, 580, 9,  'Arcane Sanctum')",
	"INSERT INTO battles VALUES (33, 11, 16, 11, 700, 10, 'Arcane Sanctum')",
	"INSERT INTO battles VALUES (34, 8,  1,  8, 1080, 12, 'Shadow Realm')",
	"INSERT INTO battles VALUES (35, 1,  8,  8,  940, 15, 'Shadow Realm')",
	"INSERT INTO battles VALUES (36, 5,  3,  5,  680, 9,  'City Arena')",
	"INSERT INTO battles VALUES (37, 15, 3,  15, 760, 11, 'City Arena')",
	"INSERT INTO battles VALUES (38, 19, 15, 19, 820, 13, 'Iron Keep')",
	"INSERT INTO battles VALUES (39, 8,  19, 19, 880, 17, 'Shadow Realm')",
	"INSERT INTO battles VALUES (40, 8,  19, 8,  960, 20, 'Shadow Realm')",
}

func execSQL(exec *executor.Executor, sql string) error {
	tokens := lexer.New(sql).Tokenize()
	stmt, err := parser.New(tokens).Parse()
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	_, err = exec.Execute(stmt)
	return err
}

func SeedHandler(db *storage.Database) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, map[string]interface{}{"error": "only POST allowed"})
			return
		}

		exec := executor.New(db)
		var errs []string
		ok := 0
		for _, sql := range seedStatements {
			if err := execSQL(exec, sql); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", sql[:min(40, len(sql))], err.Error()))
			} else {
				ok++
			}
		}

		writeJSON(w, map[string]interface{}{
			"ok":     ok,
			"errors": errs,
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
