import { IsEmail, IsNotEmpty, IsString, Length } from 'class-validator';

export class RegisterUserDto {
  @IsString()
  @Length(4, 16)
  user_name!: string;

  @IsString()
  @Length(6, 16)
  password_hash!: string;

  @IsEmail()
  @IsNotEmpty()
  email!: string;
}
