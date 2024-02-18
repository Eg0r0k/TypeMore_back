import { $Enums, Prisma } from '@prisma/client';

export class Users implements Prisma.UsersCreateInput {
  id!: string;
  updated_at!: Date;
  hashed_rt!: string | null;
  email!: string;
  user_name!: string;
  password_hash!: string;
  registration_date!: Date;
  games_played!: number;
  games_end!: number;
  best_score!: number;
  role!: $Enums.Role;
}
